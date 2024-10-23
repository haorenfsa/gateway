// Copyright Envoy Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

// This file contains code derived from Contour,
// https://github.com/projectcontour/contour
// from the source file
// https://github.com/projectcontour/contour/blob/main/internal/xds/v3/snapshotter.go
// and is provided here subject to the following:
// Copyright Project Contour Authors
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"go.uber.org/zap"

	"github.com/envoyproxy/gateway/internal/logging"
	"github.com/envoyproxy/gateway/internal/metrics"
	"github.com/envoyproxy/gateway/internal/xds/types"
)

var Hash = cachev3.IDHash{}

// SnapshotCacheWithCallbacks uses the go-control-plane SimpleCache to store snapshots of
// Envoy resources, sliced by Node ID so that we can do incremental xDS properly.
// It does this by also implementing callbacks to make sure that the cache is kept
// up to date for each new node.
//
// Having the cache also implement the callbacks is a little bit hacky, but it makes sure
// that all the required bookkeeping happens.
// TODO(youngnick): Talk to the go-control-plane maintainers and see if we can upstream
// this in a better way.
type SnapshotCacheWithCallbacks interface {
	cachev3.SnapshotCache
	serverv3.Callbacks
	GenerateNewSnapshot(string, types.XdsResources) error
}

type snapshotMap map[string]*cachev3.Snapshot

type nodeInfoMap map[int64]*corev3.Node

type streamDurationMap map[int64]time.Time

type snapshotCache struct {
	cachev3.SnapshotCache
	streamIDNodeInfo    nodeInfoMap
	streamDuration      streamDurationMap
	deltaStreamDuration streamDurationMap
	snapshotVersion     int64
	lastSnapshot        snapshotMap
	log                 *zap.SugaredLogger
	mu                  sync.Mutex
}

// GenerateNewSnapshot takes a table of resources (the output from the IR->xDS
// translator) and updates the snapshot version.
func (s *snapshotCache) GenerateNewSnapshot(irKey string, resources types.XdsResources) error {
	beginTime := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		s.log.Infow("Generated a new snapshot", "irKey", irKey, "duration", time.Since(beginTime))
	}()

	version := s.newSnapshotVersion()

	// Create a snapshot with all xDS resources.
	snapshot, err := cachev3.NewSnapshot(
		version,
		resources,
	)
	if err != nil {
		xdsSnapshotCreateTotal.WithFailure(metrics.ReasonError).Increment()
		return err
	}
	xdsSnapshotCreateTotal.WithSuccess().Increment()

	s.lastSnapshot[irKey] = snapshot

	for _, node := range s.getNodeIDs(irKey) {
		s.log.Debugf("Generating a snapshot with Node %s", node)

		if err = s.SetSnapshot(context.TODO(), node, snapshot); err != nil {
			xdsSnapshotUpdateTotal.WithFailure(metrics.ReasonError, nodeIDLabel.Value(node)).Increment()
			return err
		} else {
			xdsSnapshotUpdateTotal.WithSuccess(nodeIDLabel.Value(node)).Increment()
		}
	}

	return nil
}

// newSnapshotVersion increments the current snapshotVersion
// and returns as a string.
func (s *snapshotCache) newSnapshotVersion() string {
	// Reset the snapshotVersion if it ever hits max size.
	if s.snapshotVersion == math.MaxInt64 {
		s.snapshotVersion = 0
	}

	// Increment the snapshot version & return as string.
	s.snapshotVersion++
	return strconv.FormatInt(s.snapshotVersion, 10)
}

// NewSnapshotCache gives you a fresh SnapshotCache.
// It needs a logger that supports the go-control-plane
// required interface (Debugf, Infof, Warnf, and Errorf).
func NewSnapshotCache(ads bool, logger logging.Logger) SnapshotCacheWithCallbacks {
	// Set up the nasty wrapper hack.
	wrappedLogger := logger.Sugar()
	return &snapshotCache{
		SnapshotCache:       cachev3.NewSnapshotCache(ads, &Hash, wrappedLogger),
		log:                 wrappedLogger,
		lastSnapshot:        make(snapshotMap),
		streamIDNodeInfo:    make(nodeInfoMap),
		streamDuration:      make(streamDurationMap),
		deltaStreamDuration: make(streamDurationMap),
	}
}

// getNodeIDs retrieves the node ids from the node info map whose
// cluster field matches the ir key
func (s *snapshotCache) getNodeIDs(irKey string) []string {
	var nodeIDs []string
	for _, node := range s.streamIDNodeInfo {
		if node != nil && node.Cluster == irKey {
			nodeIDs = append(nodeIDs, node.Id)
		}
	}

	return nodeIDs
}

// OnStreamOpen and the other OnStream* functions implement the callbacks for the
// state-of-the-world stream types.
func (s *snapshotCache) OnStreamOpen(_ context.Context, streamID int64, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.streamIDNodeInfo[streamID] = nil
	s.streamDuration[streamID] = time.Now()

	return nil
}

func (s *snapshotCache) OnStreamClosed(streamID int64, node *corev3.Node) {
	// TODO: something with the node?
	s.mu.Lock()
	defer s.mu.Unlock()

	if startTime, ok := s.streamDuration[streamID]; ok {
		streamDuration := time.Since(startTime)
		xdsStreamDurationSeconds.With(
			streamIDLabel.Value(strconv.FormatInt(streamID, 10)),
			nodeIDLabel.Value(node.Id),
			isDeltaStreamLabel.Value("false"),
		).Record(streamDuration.Seconds())
	}

	delete(s.streamIDNodeInfo, streamID)
	delete(s.streamDuration, streamID)
}

func (s *snapshotCache) OnStreamRequest(streamID int64, req *discoveryv3.DiscoveryRequest) error {
	s.log.Infof("handling v3 xDS resource request, version_info %s, response_nonce %s, resource_names %v, type_url %s",
		req.VersionInfo, req.ResponseNonce, req.ResourceNames, req.GetTypeUrl())

	beginTime := time.Now()
	s.mu.Lock()
	// We could do this a little earlier than the defer, since the last half of this func is only logging
	// but that seemed like a premature optimization.
	defer s.mu.Unlock()
	lockDuration := time.Since(beginTime)
	s.log.Infof("handling v3 xDS resource request, version_info %s, response_nonce %s, resource_names %v, type_url %s, lock_duration %s",
		req.VersionInfo, req.ResponseNonce, req.ResourceNames, req.GetTypeUrl(), lockDuration)

	// It's possible that only the first discovery request will have a node ID set.
	// We also need to save the node ID to the node list anyway.
	// So check if we have a nodeID for this stream already, then set it if not.
	if s.streamIDNodeInfo[streamID] == nil {
		if req.Node.Id == "" {
			return fmt.Errorf("couldn't get the node ID from the first discovery request on stream %d", streamID)
		}
		s.log.Debugf("First discovery request on stream %d, got nodeID %s", streamID, req.Node.Id)
		s.streamIDNodeInfo[streamID] = req.Node
	}
	nodeID := s.streamIDNodeInfo[streamID].Id
	cluster := s.streamIDNodeInfo[streamID].Cluster

	var nodeVersion string

	var errorCode int32
	var errorMessage string

	// If no snapshot has been generated yet, we can't do anything, so don't mess with this request.
	// go-control-plane will respond with an empty response, then send an update when a snapshot is generated.
	if s.lastSnapshot[cluster] == nil {
		return nil
	}

	_, err := s.GetSnapshot(nodeID)
	if err != nil {
		err = s.SetSnapshot(context.TODO(), nodeID, s.lastSnapshot[cluster])
		if err != nil {
			return err
		}
	}

	if req.Node != nil {
		if bv := req.Node.GetUserAgentBuildVersion(); bv != nil && bv.Version != nil {
			nodeVersion = fmt.Sprintf("v%d.%d.%d", bv.Version.MajorNumber, bv.Version.MinorNumber, bv.Version.Patch)
		}
	}

	s.log.Debugf("Got a new request, version_info %s, response_nonce %s, nodeID %s, node_version %s", req.VersionInfo, req.ResponseNonce, nodeID, nodeVersion)

	if status := req.ErrorDetail; status != nil {
		// if Envoy rejected the last update log the details here.
		// TODO(youngnick): Handle NACK properly
		errorCode = status.Code
		errorMessage = status.Message
	}

	finishDuration := time.Since(beginTime)

	s.log.Infof("handling v3 xDS resource request, version_info %s, response_nonce %s, nodeID %s, node_version %s, resource_names %v, type_url %s, errorCode %d, errorMessage %s, lock_duration %s, finish_duration %s",
		req.VersionInfo, req.ResponseNonce,
		nodeID, nodeVersion, req.ResourceNames, req.GetTypeUrl(),
		errorCode, errorMessage, lockDuration, finishDuration)

	return nil
}

func (s *snapshotCache) OnStreamResponse(_ context.Context, streamID int64, _ *discoveryv3.DiscoveryRequest, _ *discoveryv3.DiscoveryResponse) {
	// No mutex lock required here because no writing to the cache.
	node := s.streamIDNodeInfo[streamID]
	if node == nil {
		s.log.Errorf("Tried to send a response to a node we haven't seen yet on stream %d", streamID)
	} else {
		s.log.Debugf("Sending Response on stream %d to node %s", streamID, node.Id)
	}
}

// OnDeltaStreamOpen and the other OnDeltaStream*/OnStreamDelta* functions implement
// the callbacks for the incremental xDS versions.
// Yes, the different ordering in the name is part of the go-control-plane interface.
func (s *snapshotCache) OnDeltaStreamOpen(_ context.Context, streamID int64, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log.Infof("xDS OnDeltaStreamOpen streamID %d, open at %s", streamID, time.Now())

	// Ensure that we're adding the streamID to the Node ID list.
	s.streamIDNodeInfo[streamID] = nil
	s.deltaStreamDuration[streamID] = time.Now()

	return nil
}

func (s *snapshotCache) OnDeltaStreamClosed(streamID int64, node *corev3.Node) {
	// TODO: something with the node?
	s.mu.Lock()
	defer s.mu.Unlock()

	if startTime, ok := s.deltaStreamDuration[streamID]; ok {
		deltaStreamDuration := time.Since(startTime)
		xdsStreamDurationSeconds.With(
			streamIDLabel.Value(strconv.FormatInt(streamID, 10)),
			nodeIDLabel.Value(node.Id),
			isDeltaStreamLabel.Value("true"),
		).Record(deltaStreamDuration.Seconds())
	}

	delete(s.streamIDNodeInfo, streamID)
	delete(s.deltaStreamDuration, streamID)
}

var (
	streamDeltaReqBeginTime     = map[*discoveryv3.DeltaDiscoveryRequest]time.Time{}
	streamDeltaReqBeginTimeLock sync.Mutex
)

func (s *snapshotCache) OnStreamDeltaRequest(streamID int64, req *discoveryv3.DeltaDiscoveryRequest) error {
	beginTime := time.Now()
	streamDeltaReqBeginTimeLock.Lock()
	streamDeltaReqBeginTime[req] = beginTime
	streamDeltaReqBeginTimeLock.Unlock()

	s.log.Infof("handling v3 xDS delta resource request, stream %d, sub %s, unsub %s, url %s, req %p"
		streamID, req.ResourceNamesSubscribe, req.ResourceNamesUnsubscribe,
		req.GetTypeUrl(), req)

	s.mu.Lock()
	// We could do this a little earlier than with a defer, since the last half of this func is logging
	// but that seemed like a premature optimization.
	defer s.mu.Unlock()

	defer func(){
		lockDuration := time.Since(beginTime)
		s.log.Infof("v3 xDS delta resource request cache lock duration, stream %d, sub %s, unsub %s, url %s, req %p, lock_duration %s",
			streamID, req.ResourceNamesSubscribe, req.ResourceNamesUnsubscribe,
			req.GetTypeUrl(), req, lockDuration)
	}()

	var nodeVersion string
	var errorCode int32
	var errorMessage string

	// It's possible that only the first incremental discovery request will have a node ID set.
	// We also need to save the node ID to the node list anyway.
	// So check if we have a nodeID for this stream already, then set it if not.
	node := s.streamIDNodeInfo[streamID]
	if node == nil {
		if req.Node.Id == "" {
			return fmt.Errorf("couldn't get the node ID from the first incremental discovery request on stream %d", streamID)
		}
		s.log.Debugf("First incremental discovery request on stream %d, got nodeID %s", streamID, req.Node.Id)
		s.streamIDNodeInfo[streamID] = req.Node
	}
	nodeID := s.streamIDNodeInfo[streamID].Id
	cluster := s.streamIDNodeInfo[streamID].Cluster

	// If no snapshot has been written into the snapshotCache yet, we can't do anything, so don't mess with
	// this request. go-control-plane will respond with an empty response, then send an update when a
	// snapshot is generated.
	if s.lastSnapshot[cluster] == nil {
		return nil
	}

	_, err := s.GetSnapshot(nodeID)
	if err != nil {
		err = s.SetSnapshot(context.TODO(), nodeID, s.lastSnapshot[cluster])
		if err != nil {
			return err
		}
	}

	if req.Node != nil {
		if bv := req.Node.GetUserAgentBuildVersion(); bv != nil && bv.Version != nil {
			nodeVersion = fmt.Sprintf("v%d.%d.%d", bv.Version.MajorNumber, bv.Version.MinorNumber, bv.Version.Patch)
		}
	}

	s.log.Debugf("Got a new request, response_nonce %s, nodeID %s, node_version %s",
		req.ResponseNonce, nodeID, nodeVersion)
	if status := req.ErrorDetail; status != nil {
		// if Envoy rejected the last update log the details here.
		// TODO(youngnick): Handle NACK properly
		errorCode = status.Code
		errorMessage = status.Message
	}

	finishDuration := time.Since(beginTime)
	s.log.Debugf("handling v3 xDS delta resource request, response_nonce %s, nodeID %s, node_version %s, resource_names_subscribe %v, resource_names_unsubscribe %v, type_url %s, errorCode %d, errorMessage %s, finish_duration %s",
		req.ResponseNonce,
		nodeID, nodeVersion,
		req.ResourceNamesSubscribe, req.ResourceNamesUnsubscribe,
		req.GetTypeUrl(),
		errorCode, errorMessage, finishDuration)

	return nil
}

func (s *snapshotCache) OnStreamDeltaResponse(streamID int64, req *discoveryv3.DeltaDiscoveryRequest, _ *discoveryv3.DeltaDiscoveryResponse) {
	// No mutex lock required here because no writing to the cache.
	streamDeltaReqBeginTimeLock.Lock()
	beginTime, ok := streamDeltaReqBeginTime[req]
	delete(streamDeltaReqBeginTime, req)
	streamDeltaReqBeginTimeLock.Unlock()
	if !ok {
		s.log.Errorf("xDS unexpected req %s", req)
	}
	deltaStreamDuration := time.Since(beginTime)
	s.log.Infof("handling v3 xDS delta resource response, stream %d, sub %s, unsub %s, url %s, req %p, duration %s",
		streamID, req.ResourceNamesSubscribe, req.ResourceNamesUnsubscribe,
		req.GetTypeUrl(), req, deltaStreamDuration)

	node := s.streamIDNodeInfo[streamID]
	if node == nil {
		s.log.Errorf("Tried to send a response to a node we haven't seen yet on stream %d", streamID)
	} else {
		s.log.Debugf("Sending Incremental Response on stream %d to node %s", streamID, node.Id)
	}
}

func (s *snapshotCache) OnFetchRequest(_ context.Context, req *discoveryv3.DiscoveryRequest) error {
	return nil
}

func (s *snapshotCache) OnFetchResponse(req *discoveryv3.DiscoveryRequest, _ *discoveryv3.DiscoveryResponse) {
	// No mutex lock required here because no writing to the cache.
}
