package kubernetes

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/Trendyol/go-dcp/config"

	"github.com/Trendyol/go-dcp/logger"
	"github.com/Trendyol/go-dcp/membership"
)

type statefulSetMembership struct {
	info *membership.Model
}

func (s *statefulSetMembership) GetInfo() *membership.Model {
	return s.info
}

func (s *statefulSetMembership) Close() {
}

func getPodOrdinalFromHostname() (int, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return -1, err
	}

	separatorIndex := strings.LastIndex(hostname, "-")

	if separatorIndex == -1 {
		return -1, fmt.Errorf("hostname is not in statefulSet format")
	}

	podOrdinal, err := strconv.Atoi(hostname[separatorIndex+1:])
	if err != nil {
		return -1, err
	}

	return podOrdinal, nil
}

func NewStatefulSetMembership(config *config.Dcp) membership.Membership {
	podOrdinal, err := getPodOrdinalFromHostname()
	if err != nil {
		logger.Log.Error("error while get pod ordinal from hostname: %v", err)
		panic(err)
	}

	memberNumber := podOrdinal + 1

	if memberNumber > config.Dcp.Group.Membership.TotalMembers {
		err := fmt.Errorf("memberNumber is greater than totalMembers")
		logger.Log.Error("memberNumber: %v, totalMembers: %v, err: %v", memberNumber, config.Dcp.Group.Membership.TotalMembers, err)
		panic(err)
	}

	return &statefulSetMembership{
		info: &membership.Model{
			MemberNumber: memberNumber,
			TotalMembers: config.Dcp.Group.Membership.TotalMembers,
		},
	}
}
