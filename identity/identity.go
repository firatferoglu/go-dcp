package identity

import (
	"os"

	jsoniter "github.com/json-iterator/go"

	"github.com/Trendyol/go-dcp-client/logger"
)

type Identity struct {
	IP   string
	Name string
}

func (k *Identity) String() string {
	str, err := jsoniter.Marshal(k)
	if err != nil {
		logger.ErrorLog.Printf("error while marshalling identity: %v", err)
		panic(err)
	}

	return string(str)
}

func (k *Identity) Equal(other *Identity) bool {
	return k.IP == other.IP && k.Name == other.Name
}

func NewIdentityFromStr(str string) *Identity {
	var identity Identity

	err := jsoniter.Unmarshal([]byte(str), &identity)
	if err != nil {
		logger.ErrorLog.Printf("error while unmarshalling identity: %v", err)
		panic(err)
	}

	return &identity
}

func NewIdentityFromEnv() *Identity {
	return &Identity{
		IP:   os.Getenv("POD_IP"),
		Name: os.Getenv("POD_NAME"),
	}
}
