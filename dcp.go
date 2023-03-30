package godcpclient

import (
	"errors"
	"os"
	"os/signal"
	"reflect"
	"syscall"
	"time"

	"github.com/Trendyol/go-dcp-client/membership"

	"github.com/Trendyol/go-dcp-client/api"

	"github.com/Trendyol/go-dcp-client/metadata"

	"github.com/Trendyol/go-dcp-client/stream"

	"github.com/Trendyol/go-dcp-client/couchbase"

	"github.com/Trendyol/go-dcp-client/models"

	"github.com/Trendyol/go-dcp-client/helpers"
	"github.com/Trendyol/go-dcp-client/logger"
	"github.com/Trendyol/go-dcp-client/servicediscovery"
)

type Dcp interface {
	Start()
	Close()
	Commit()
	GetConfig() *helpers.Config
	SetMetadata(metadata metadata.Metadata)
}

type dcp struct {
	client            couchbase.Client
	stream            stream.Stream
	api               api.API
	leaderElection    stream.LeaderElection
	vBucketDiscovery  stream.VBucketDiscovery
	serviceDiscovery  servicediscovery.ServiceDiscovery
	listener          models.Listener
	apiShutdown       chan struct{}
	cancelCh          chan os.Signal
	stopCh            chan struct{}
	healCheckFailedCh chan struct{}
	config            *helpers.Config
	healthCheckTicker *time.Ticker
	metadata          metadata.Metadata
}

func (s *dcp) getCollectionIDs() map[uint32]string {
	collectionIDs := map[uint32]string{}

	if s.config.IsCollectionModeEnabled() {
		ids, err := s.client.GetCollectionIDs(s.config.ScopeName, s.config.CollectionNames)
		if err != nil {
			logger.ErrorLog.Printf("cannot get collection ids: %v", err)
			panic(err)
		}

		collectionIDs = ids
	}

	return collectionIDs
}

func (s *dcp) startHealthCheck() {
	s.healthCheckTicker = time.NewTicker(s.config.HealthCheck.Interval)

	go func() {
		for range s.healthCheckTicker.C {
			if err := s.client.Ping(); err != nil {
				logger.ErrorLog.Printf("health check failed: %v", err)
				s.healthCheckTicker.Stop()
				s.healCheckFailedCh <- struct{}{}
				break
			}
		}
	}()
}

func (s *dcp) stopHealthCheck() {
	s.healthCheckTicker.Stop()
}

func (s *dcp) SetMetadata(metadata metadata.Metadata) {
	s.metadata = metadata
}

func (s *dcp) Start() {
	if s.metadata == nil {
		switch {
		case s.config.IsCouchbaseMetadata():
			s.metadata = couchbase.NewCBMetadata(s.client, s.config)
		case s.config.IsFileMetadata():
			s.metadata = metadata.NewFSMetadata(s.config)
		default:
			panic(errors.New("invalid metadata type"))
		}
	}

	if s.config.Metadata.ReadOnly {
		s.metadata = metadata.NewReadMetadata(s.metadata)
	}

	logger.Log.Printf("using %v metadata", reflect.TypeOf(s.metadata))

	infoHandler := membership.NewHandler()

	vBuckets := s.client.GetNumVBuckets()

	s.vBucketDiscovery = stream.NewVBucketDiscovery(s.client, s.config, vBuckets, infoHandler)

	s.stream = stream.NewStream(s.client, s.metadata, s.config, s.vBucketDiscovery, s.listener, s.getCollectionIDs(), s.stopCh)

	if s.config.LeaderElection.Enabled {
		s.serviceDiscovery = servicediscovery.NewServiceDiscovery(s.config, infoHandler)
		s.serviceDiscovery.StartHeartbeat()
		s.serviceDiscovery.StartMonitor()

		s.leaderElection = stream.NewLeaderElection(s.config, s.serviceDiscovery, infoHandler)
		s.leaderElection.Start()
	}

	s.stream.Open()

	infoHandler.Subscribe(func(new *membership.Model) {
		s.stream.Rebalance()
	})

	if s.config.API.Enabled {
		go func() {
			go func() {
				<-s.apiShutdown
				s.api.Shutdown()
			}()

			s.api = api.NewAPI(s.config, s.client, s.stream, s.serviceDiscovery, s.vBucketDiscovery)
			s.api.Listen()
		}()
	}

	signal.Notify(s.cancelCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGABRT, syscall.SIGQUIT)

	if s.config.HealthCheck.Enabled {
		s.startHealthCheck()
	}

	logger.Log.Printf("dcp stream started")

	if s.config.Debug {
		helpers.LogMemUsage()
	}

	select {
	case <-s.stopCh:
	case <-s.cancelCh:
	case <-s.healCheckFailedCh:
	}
}

func (s *dcp) Close() {
	if s.config.HealthCheck.Enabled {
		s.stopHealthCheck()
	}
	s.vBucketDiscovery.Close()

	if s.config.Checkpoint.Type == helpers.CheckpointTypeAuto {
		s.stream.Save()
	}
	s.stream.Close()

	if s.config.LeaderElection.Enabled {
		s.leaderElection.Stop()

		s.serviceDiscovery.StopMonitor()
		s.serviceDiscovery.StopHeartbeat()
	}

	if s.api != nil && s.config.API.Enabled {
		s.apiShutdown <- struct{}{}
	}

	s.client.DcpClose()
	s.client.Close()

	logger.Log.Printf("dcp stream closed")
}

func (s *dcp) Commit() {
	s.stream.Save()
}

func (s *dcp) GetConfig() *helpers.Config {
	return s.config
}

func newDcp(config *helpers.Config, listener models.Listener) (Dcp, error) {
	client := couchbase.NewClient(config)

	err := client.Connect()
	if err != nil {
		return nil, err
	}

	err = client.DcpConnect()

	if err != nil {
		return nil, err
	}

	return &dcp{
		client:            client,
		listener:          listener,
		config:            config,
		apiShutdown:       make(chan struct{}, 1),
		cancelCh:          make(chan os.Signal, 1),
		stopCh:            make(chan struct{}, 1),
		healCheckFailedCh: make(chan struct{}, 1),
	}, nil
}

// NewDcp creates a new DCP client
//
// config: path to a configuration file or a configuration struct
// listener is a callback function that will be called when a mutation, deletion or expiration event occurs
func NewDcp(configPath string, listener models.Listener) (Dcp, error) {
	config := helpers.NewConfig(helpers.Name, configPath)
	return newDcp(config, listener)
}

func NewDcpWithLoggers(configPath string, listener models.Listener, infoLogger logger.Logger, errorLogger logger.Logger) (Dcp, error) {
	logger.SetLogger(infoLogger)
	logger.SetErrorLogger(errorLogger)

	return NewDcp(configPath, listener)
}
