// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package history

import (
	"sync/atomic"
	"time"

	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/dynamicconfig"
	"github.com/uber/cadence/common/log/tag"
	"github.com/uber/cadence/common/service"
	"github.com/uber/cadence/service/history/config"
	"github.com/uber/cadence/service/history/resource"
)

// Service represents the cadence-history service
type Service struct {
	resource.Resource

	status  int32
	handler *handlerImpl
	stopC   chan struct{}
	params  *service.BootstrapParams
	config  *config.Config
}

// NewService builds a new cadence-history service
func NewService(
	params *service.BootstrapParams,
) (resource.Resource, error) {
	serviceConfig := config.New(
		dynamicconfig.NewCollection(
			params.DynamicConfig,
			params.Logger,
			dynamicconfig.ClusterNameFilter(params.ClusterMetadata.GetCurrentClusterName()),
		),
		params.PersistenceConfig.NumHistoryShards,
		params.PersistenceConfig.DefaultStoreType(),
		params.PersistenceConfig.IsAdvancedVisibilityConfigExist())

	params.PersistenceConfig.HistoryMaxConns = serviceConfig.HistoryMgrNumConns()

	serviceResource, err := resource.New(
		params,
		common.HistoryServiceName,
		serviceConfig,
	)
	if err != nil {
		return nil, err
	}

	return &Service{
		Resource: serviceResource,
		status:   common.DaemonStatusInitialized,
		stopC:    make(chan struct{}),
		params:   params,
		config:   serviceConfig,
	}, nil
}

// Start starts the service
func (s *Service) Start() {
	if !atomic.CompareAndSwapInt32(&s.status, common.DaemonStatusInitialized, common.DaemonStatusStarted) {
		return
	}

	logger := s.GetLogger()
	logger.Info("elastic search config", tag.ESConfig(s.params.ESConfig))
	logger.Info("history starting")

	s.handler = NewHandler(s.Resource, s.config)

	thriftHandler := NewThriftHandler(s.handler)
	thriftHandler.register(s.GetDispatcher())

	grpcHandler := newGRPCHandler(s.handler)
	grpcHandler.register(s.GetDispatcher())

	// must start resource first
	s.Resource.Start()
	s.handler.Start()

	logger.Info("history started")

	<-s.stopC
}

// Stop stops the service
func (s *Service) Stop() {
	if !atomic.CompareAndSwapInt32(&s.status, common.DaemonStatusStarted, common.DaemonStatusStopped) {
		return
	}

	// initiate graceful shutdown :
	// 1. remove self from the membership ring
	// 2. wait for other members to discover we are going down
	// 3. stop acquiring new shards (periodically or based on other membership changes)
	// 4. wait for shard ownership to transfer (and inflight requests to drain) while still accepting new requests
	// 5. Reject all requests arriving at rpc handler to avoid taking on more work except for RespondXXXCompleted and
	//    RecordXXStarted APIs - for these APIs, most of the work is already one and rejecting at last stage is
	//    probably not that desirable. If the shard is closed, these requests will fail anyways.
	// 6. wait for grace period
	// 7. force stop the whole world and return

	const gossipPropagationDelay = 400 * time.Millisecond
	const shardOwnershipTransferDelay = 5 * time.Second
	const gracePeriod = 2 * time.Second

	remainingTime := s.config.ShutdownDrainDuration()

	s.GetLogger().Info("ShutdownHandler: Evicting self from membership ring")
	s.GetMembershipMonitor().EvictSelf()

	s.GetLogger().Info("ShutdownHandler: Waiting for others to discover I am unhealthy")
	remainingTime = s.sleep(gossipPropagationDelay, remainingTime)

	s.GetLogger().Info("ShutdownHandler: Initiating shardController shutdown")
	s.handler.controller.PrepareToStop()
	s.GetLogger().Info("ShutdownHandler: Waiting for traffic to drain")
	remainingTime = s.sleep(shardOwnershipTransferDelay, remainingTime)

	s.GetLogger().Info("ShutdownHandler: No longer taking rpc requests")
	s.handler.PrepareToStop()
	remainingTime = s.sleep(gracePeriod, remainingTime)

	close(s.stopC)

	s.handler.Stop()
	s.Resource.Stop()

	s.GetLogger().Info("history stopped")
}

// sleep sleeps for the minimum of desired and available duration
// returns the remaining available time duration
func (s *Service) sleep(desired time.Duration, available time.Duration) time.Duration {
	d := common.MinDuration(desired, available)
	if d > 0 {
		time.Sleep(d)
	}
	return available - d
}
