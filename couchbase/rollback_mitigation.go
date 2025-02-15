package couchbase

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/asaskevich/EventBus"

	"github.com/couchbase/gocbcore/v10"

	"github.com/Trendyol/go-dcp/wrapper"

	"github.com/Trendyol/go-dcp/models"

	"github.com/Trendyol/go-dcp/config"

	"github.com/Trendyol/go-dcp/helpers"
	"github.com/Trendyol/go-dcp/logger"
)

type RollbackMitigation interface {
	Start()
	Stop()
}

type vbUUIDAndSeqNo struct {
	vbUUID gocbcore.VbUUID
	seqNo  gocbcore.SeqNo
	absent bool
}

func (v *vbUUIDAndSeqNo) SetAbsent() {
	v.absent = true
}

func (v *vbUUIDAndSeqNo) IsAbsent() bool {
	return v.absent
}

func (v *vbUUIDAndSeqNo) SetSeqNo(seqNo gocbcore.SeqNo) {
	v.seqNo = seqNo
}

func (v *vbUUIDAndSeqNo) SetVbUUID(vbUUID gocbcore.VbUUID) {
	v.vbUUID = vbUUID
}

type rollbackMitigation struct {
	client             Client
	config             *config.Dcp
	bus                EventBus.Bus
	configSnapshot     *gocbcore.ConfigSnapshot
	persistedSeqNos    *wrapper.ConcurrentSwissMap[uint16, []*vbUUIDAndSeqNo]
	vbUUIDMap          *wrapper.ConcurrentSwissMap[uint16, gocbcore.VbUUID]
	configWatchTimer   *time.Ticker
	observeTimer       *time.Ticker
	observeCloseCh     chan struct{}
	observeCloseDoneCh chan struct{}
	vbIds              []uint16
	activeGroupID      int
	closed             bool
}

func (r *rollbackMitigation) getRevEpochAndID(snapshot *gocbcore.ConfigSnapshot) (int64, int64) {
	state := reflect.ValueOf(snapshot).Elem().FieldByName("state")
	routeCfg := state.Elem().FieldByName("routeCfg")

	revEpoch := routeCfg.FieldByName("revEpoch").Int()
	revID := routeCfg.FieldByName("revID").Int()

	return revEpoch, revID
}

func (r *rollbackMitigation) configWatch() {
	snapshot, err := r.client.GetConfigSnapshot()

	if err == nil && (r.configSnapshot == nil || r.isConfigSnapshotNewerThan(snapshot)) {
		r.configSnapshot = snapshot
		r.reconfigure()
	}
}

func (r *rollbackMitigation) isConfigSnapshotNewerThan(newConfigSnapshot *gocbcore.ConfigSnapshot) bool {
	oldEpoch, oldRevID := r.getRevEpochAndID(r.configSnapshot)
	newEpoch, newRevID := r.getRevEpochAndID(newConfigSnapshot)

	if newEpoch < oldEpoch {
		return false
	} else if newEpoch == oldEpoch {
		if newRevID == oldRevID {
			return false
		} else if newRevID < oldRevID {
			return false
		}
	}

	return true
}

func (r *rollbackMitigation) observeVbID(
	vbID uint16,
	replica int,
	vbUUID gocbcore.VbUUID,
	callback func(*gocbcore.ObserveVbResult, error),
) { //nolint:unused
	_, err := r.client.GetAgent().ObserveVb(gocbcore.ObserveVbOptions{
		VbID:       vbID,
		ReplicaIdx: replica,
		VbUUID:     vbUUID,
	}, callback)
	if err != nil {
		logger.Log.Error("observeVBID error for vbId: %v, replica:%v, vbUUID: %v, err: %v", vbID, replica, vbUUID, err)
		callback(nil, err)
	}
}

func (r *rollbackMitigation) getMinSeqNo(vbID uint16) gocbcore.SeqNo { //nolint:unused
	startIndex := -1
	replicas, _ := r.persistedSeqNos.Load(vbID)

	for idx, replica := range replicas {
		if !replica.IsAbsent() {
			startIndex = idx
			break
		}
	}

	if startIndex == -1 {
		logger.Log.Error("all replicas absent")
		return 0
	}

	vbUUID := replicas[startIndex].vbUUID
	minSeqNo := replicas[startIndex].seqNo

	for idx := startIndex + 1; idx < len(replicas); idx++ {
		replica := replicas[idx]
		if replica.IsAbsent() {
			continue
		}

		if vbUUID != replica.vbUUID {
			logger.Log.Debug("vbUUID mismatch %v != %v for %v index of %v", vbUUID, replica.vbUUID, idx, len(replicas))
			return 0
		}

		if minSeqNo > replica.seqNo {
			minSeqNo = replica.seqNo
		}
	}

	return minSeqNo
}

func (r *rollbackMitigation) markAbsentInstances() error { //nolint:unused
	var outerError error

	r.persistedSeqNos.Range(func(vbID uint16, replicas []*vbUUIDAndSeqNo) bool {
		for idx, replica := range replicas {
			serverIndex, err := r.configSnapshot.VbucketToServer(vbID, uint32(idx))
			if err != nil {
				if errors.Is(err, gocbcore.ErrInvalidReplica) {
					logger.Log.Debug("invalid replica of vbId: %v, replica: %v, err: %v", vbID, idx, err)
					replica.SetAbsent()
				} else {
					outerError = err
					return false
				}
			} else {
				if serverIndex < 0 {
					logger.Log.Debug("invalid server index of vbId: %v, replica: %v, serverIndex: %v", vbID, idx, serverIndex)
					replica.SetAbsent()
				}
			}
		}

		return true
	})

	return outerError
}

func (r *rollbackMitigation) startObserve(groupID int) {
	r.vbUUIDMap = wrapper.CreateConcurrentSwissMap[uint16, gocbcore.VbUUID](1024)

	r.loadVbUUIDMap()

	r.observeTimer = time.NewTicker(r.config.RollbackMitigation.Interval)
	for {
		select {
		case <-r.observeTimer.C:
			r.persistedSeqNos.Range(func(vbID uint16, replicas []*vbUUIDAndSeqNo) bool {
				for idx, replica := range replicas {
					if replica.IsAbsent() {
						continue
					}

					if r.closed || r.activeGroupID != groupID {
						logger.Log.Debug("closed(%v) or groupID(%v!=%v) changed on startObserve", r.closed, r.activeGroupID, groupID)
						return false
					}

					vbUUID, _ := r.vbUUIDMap.Load(vbID)
					r.observe(vbID, idx, groupID, vbUUID)
				}

				return true
			})
		case <-r.observeCloseCh:
			logger.Log.Debug("observe close triggered")
			r.observeCloseDoneCh <- struct{}{}
			return
		}
	}
}

func (r *rollbackMitigation) loadVbUUIDMap() {
	r.persistedSeqNos.Range(func(vbID uint16, _ []*vbUUIDAndSeqNo) bool {
		failoverLogs, err := r.client.GetFailoverLogs(vbID)
		if err != nil {
			panic(err)
		}

		r.vbUUIDMap.Store(vbID, failoverLogs[0].VbUUID)

		var failoverInfos []string
		for index, failoverLog := range failoverLogs {
			failoverInfos = append(
				failoverInfos,
				fmt.Sprintf("index: %v, vbUUID: %v, seqNo: %v", index, failoverLog.VbUUID, failoverLog.SeqNo),
			)
		}

		logger.Log.Debug(
			"observing vbID: %v, vbUUID: %v, failoverInfo: %v",
			vbID, failoverLogs[0].VbUUID, strings.Join(failoverInfos, ", "),
		)

		return true
	})
}

func (r *rollbackMitigation) reconfigure() {
	logger.Log.Debug("reconfigure triggerred")

	if r.observeTimer != nil {
		r.observeTimer.Stop()
		r.observeCloseCh <- struct{}{}
		<-r.observeCloseDoneCh
		logger.Log.Debug("observe close done")
	}

	r.activeGroupID++
	logger.Log.Info("new cluster config received, groupId = %v", r.activeGroupID)

	r.reset()
	err := r.markAbsentInstances()
	if err != nil {
		panic(err)
	}

	go r.startObserve(r.activeGroupID)
}

func (r *rollbackMitigation) observe(vbID uint16, replica int, groupID int, vbUUID gocbcore.VbUUID) {
	r.observeVbID(vbID, replica, vbUUID, func(result *gocbcore.ObserveVbResult, err error) {
		if r.closed || r.activeGroupID != groupID {
			logger.Log.Debug("closed(%v) or groupID(%v!=%v) changed on observe", r.closed, r.activeGroupID, groupID)
			return
		}

		if err != nil {
			if errors.Is(err, gocbcore.ErrTemporaryFailure) {
				logger.Log.Error("error while observe: %v", err)
				return
			} else {
				panic(err)
			}
		}

		replicas, ok := r.persistedSeqNos.Load(vbID)
		if !ok {
			logger.Log.Error("replicas of vbID: %v not found", vbID)
		}

		if len(replicas) > replica {
			replicas[replica].SetSeqNo(result.PersistSeqNo)
			replicas[replica].SetVbUUID(result.VbUUID)

			r.bus.Publish(helpers.PersistSeqNoChangedBusEventName, models.PersistSeqNo{
				VbID:  vbID,
				SeqNo: r.getMinSeqNo(vbID),
			})

			if vbUUID != result.VbUUID {
				r.vbUUIDMap.Store(vbID, result.VbUUID)
			}
		} else {
			logger.Log.Error("replica: %v not found", replica)
		}
	})
}

func (r *rollbackMitigation) reset() {
	replicas, err := r.configSnapshot.NumReplicas()
	if err != nil {
		panic(err)
	}

	r.persistedSeqNos = wrapper.CreateConcurrentSwissMap[uint16, []*vbUUIDAndSeqNo](1024)

	for _, vbID := range r.vbIds {
		replicaArr := make([]*vbUUIDAndSeqNo, replicas+1)
		for j := 0; j <= replicas; j++ {
			replicaArr[j] = &vbUUIDAndSeqNo{}
		}

		r.persistedSeqNos.Store(vbID, replicaArr)
	}
}

func (r *rollbackMitigation) waitFirstConfig() error {
	opm := NewAsyncOp(context.Background())

	ch := make(chan error)

	op, err := r.client.GetAgent().WaitForConfigSnapshot(
		time.Now().Add(r.config.ConnectionTimeout),
		gocbcore.WaitForConfigSnapshotOptions{},
		func(result *gocbcore.WaitForConfigSnapshotResult, err error) {
			r.configSnapshot = result.Snapshot

			opm.Resolve()

			ch <- err
		},
	)

	err = opm.Wait(op, err)
	if err != nil {
		return err
	}

	return <-ch
}

func (r *rollbackMitigation) Start() {
	logger.Log.Info("rollback mitigation will start with %v interval", r.config.RollbackMitigation.Interval)

	err := r.waitFirstConfig()
	if err != nil {
		logger.Log.Error("cannot get first config: %v", err)
		panic(err)
	}

	r.reconfigure()

	go func() {
		r.configWatchTimer = time.NewTicker(r.config.RollbackMitigation.ConfigWatchInterval)
		for range r.configWatchTimer.C {
			r.configWatch()
		}
	}()
}

func (r *rollbackMitigation) Stop() {
	r.closed = true
	if r.observeTimer != nil {
		r.observeTimer.Stop()
		r.observeCloseCh <- struct{}{}
		<-r.observeCloseDoneCh
	}

	if r.configWatchTimer != nil {
		r.configWatchTimer.Stop()
	}

	logger.Log.Info("rollback mitigation stopped")
}

func NewRollbackMitigation(client Client, config *config.Dcp, vbIds []uint16, bus EventBus.Bus) RollbackMitigation {
	return &rollbackMitigation{
		client:             client,
		config:             config,
		vbIds:              vbIds,
		bus:                bus,
		observeCloseCh:     make(chan struct{}, 1),
		observeCloseDoneCh: make(chan struct{}, 1),
	}
}
