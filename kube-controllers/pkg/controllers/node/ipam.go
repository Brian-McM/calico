// Copyright (c) 2025 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package node

import (
	"context"
	"fmt"
	"math"
	"net"
	"strings"
	"time"

	apiv3 "github.com/projectcalico/api/pkg/apis/projectcalico/v3"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	v1lister "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/projectcalico/calico/kube-controllers/pkg/config"
	"github.com/projectcalico/calico/kube-controllers/pkg/controllers/flannelmigration"
	"github.com/projectcalico/calico/kube-controllers/pkg/controllers/utils"
	libapiv3 "github.com/projectcalico/calico/libcalico-go/lib/apis/v3"
	bapi "github.com/projectcalico/calico/libcalico-go/lib/backend/api"
	"github.com/projectcalico/calico/libcalico-go/lib/backend/k8s/conversion"
	"github.com/projectcalico/calico/libcalico-go/lib/backend/model"
	client "github.com/projectcalico/calico/libcalico-go/lib/clientv3"
	cerrors "github.com/projectcalico/calico/libcalico-go/lib/errors"
	"github.com/projectcalico/calico/libcalico-go/lib/ipam"
	cnet "github.com/projectcalico/calico/libcalico-go/lib/net"
	"github.com/projectcalico/calico/libcalico-go/lib/options"
)

var (
	// Multidimensional metrics, with a vector for each pool to allow resets by pool when handling pool deletion and
	// refreshing metrics. See https://github.com/prometheus/client_golang/issues/834, option 3.
	inUseAllocationGauges    map[string]*prometheus.GaugeVec
	borrowedAllocationGauges map[string]*prometheus.GaugeVec
	blocksGauges             map[string]*prometheus.GaugeVec
	gcCandidateGauges        map[string]*prometheus.GaugeVec
	gcReclamationCounters    map[string]*prometheus.CounterVec

	// Single dimension metrics. Legacy metrics are replaced by multidimensional equivalents above. Retain for
	// backwards compatibility.
	poolSizeGauge          *prometheus.GaugeVec
	legacyAllocationsGauge *prometheus.GaugeVec
	legacyBlocksGauge      *prometheus.GaugeVec
	legacyBorrowedGauge    *prometheus.GaugeVec
)

const (
	// Used to label an allocation that does not have its node attribute set.
	unknownNodeLabel = "unknown_node"

	// key for ratelimited sync retries.
	retryKey = "ipamSyncRetry"
)

func init() {
	// Pool vectors will be registered and unregistered dynamically as pools are managed.
	inUseAllocationGauges = make(map[string]*prometheus.GaugeVec)
	borrowedAllocationGauges = make(map[string]*prometheus.GaugeVec)
	blocksGauges = make(map[string]*prometheus.GaugeVec)
	gcCandidateGauges = make(map[string]*prometheus.GaugeVec)
	gcReclamationCounters = make(map[string]*prometheus.CounterVec)

	// Register the unknown pool explicitly.
	registerMetricVectorsForPool(unknownPoolLabel)

	poolSizeGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ipam_ippool_size",
		Help: "Total number of addresses in the IP Pool",
	}, []string{"ippool"})
	prometheus.MustRegister(poolSizeGauge)

	// Total IP allocations.
	legacyAllocationsGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ipam_allocations_per_node",
		Help: "Number of IPs allocated",
	}, []string{"node"})
	prometheus.MustRegister(legacyAllocationsGauge)

	// Borrowed IPs.
	legacyBorrowedGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ipam_allocations_borrowed_per_node",
		Help: "Number of allocated IPs that are from non-affine blocks.",
	}, []string{"node"})
	prometheus.MustRegister(legacyBorrowedGauge)

	// Blocks per-node.
	legacyBlocksGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ipam_blocks_per_node",
		Help: "Number of blocks in IPAM",
	}, []string{"node"})
	prometheus.MustRegister(legacyBlocksGauge)
}

type rateLimiterItemKey struct {
	Type string
	Name string
}

func NewIPAMController(cfg config.NodeControllerConfig, c client.Interface, cs kubernetes.Interface, pi, ni cache.Indexer) *IPAMController {
	var leakGracePeriod *time.Duration
	if cfg.LeakGracePeriod != nil {
		leakGracePeriod = &cfg.LeakGracePeriod.Duration
	}

	syncChan := make(chan interface{}, 1)

	// Create a rate limited that compares two distinct limiters and uses the max. This rate limiter is used
	// only to control the retry rate of whole IPAM sync executions.
	rl := workqueue.NewTypedMaxOfRateLimiter(
		// Exponential backoff, starting at 5ms and max of 30s.
		workqueue.NewTypedItemExponentialFailureRateLimiter[any](5*time.Millisecond, 30*time.Second),
		// A bucket limiter, bursting to 100 with a limit of 10 per second.
		&workqueue.TypedBucketRateLimiter[any]{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
	)

	// Retry controller takes the rate limiter as input and schedules events to the channel
	// when the desired duration has passed.
	retryController := utils.NewRetryController(
		// Use the ratelimiter above to calculate when retries should occur.
		func() time.Duration { return rl.When(retryKey) },
		// Kick the sync channel when the retry timer pops.
		func() { kick(syncChan) },
		// Clear the ratelimiter on success.
		func() { rl.Forget(retryKey) },
	)

	return &IPAMController{
		client:    c,
		clientset: cs,
		config:    cfg,

		syncChan: syncChan,

		podLister:  v1lister.NewPodLister(pi),
		nodeLister: v1lister.NewNodeLister(ni),

		nodeDeletionChan: make(chan struct{}, utils.BatchUpdateSize),
		podDeletionChan:  make(chan *v1.Pod, utils.BatchUpdateSize),

		// Buffered channels for potentially bursty channels.
		syncerUpdates: make(chan interface{}, utils.BatchUpdateSize),

		allBlocks:                   make(map[string]model.KVPair),
		allocationsByBlock:          make(map[string]map[string]*allocation),
		allocationState:             newAllocationState(),
		handleTracker:               newHandleTracker(),
		kubernetesNodesByCalicoName: make(map[string]string),
		confirmedLeaks:              make(map[string]*allocation),
		nodesByBlock:                make(map[string]string),
		blocksByNode:                make(map[string]map[string]bool),
		emptyBlocks:                 make(map[string]string),
		poolManager:                 newPoolManager(),
		datastoreReady:              true,
		consolidationWindow:         1 * time.Second,

		// Track blocks which we might want to release.
		blockReleaseTracker: newBlockReleaseTracker(leakGracePeriod),

		// For unit testing purposes.
		pauseRequestChannel: make(chan pauseRequest),

		// Retries.
		retryController: retryController,
	}
}

type IPAMController struct {
	client     client.Interface
	clientset  kubernetes.Interface
	podLister  v1lister.PodLister
	nodeLister v1lister.NodeLister
	config     config.NodeControllerConfig

	syncStatus bapi.SyncStatus

	// kubernetesNodesByCalicoName is a local cache that maps Calico nodes to their Kubernetes node name.
	kubernetesNodesByCalicoName map[string]string

	// syncChan triggers processing in response to an update.
	syncChan chan interface{}

	// For update / deletion events from the syncer.
	syncerUpdates chan interface{}

	// Raw block storage, keyed by CIDR.
	allBlocks map[string]model.KVPair

	// allocationState is the primary in-memory representation of IPAM allocations used by the garbage collector.
	allocationState *allocationState

	// allocationsByBlock maps a block CIDR to a map of allocations keyed by a unique identifier.
	// It's used as a helper to efficiently update allocationState, as well as for populating metrics.
	allocationsByBlock map[string]map[string]*allocation

	// handleTracker is used to track which handles are in use, which are potentially leaked, and which are confirmed as leaks.
	handleTracker *handleTracker

	// confirmedLeaks indexes allocations that are confirmed to be leaks and are awaiting cleanup.
	confirmedLeaks map[string]*allocation

	// nodesByBlock and blocksByNode are used together to decide when block affinities are redundant and safe to release.
	nodesByBlock map[string]string
	blocksByNode map[string]map[string]bool

	// blockReleaseTracker is used to track blocks that are candidates for GC due to both redundancy and inactivity.
	blockReleaseTracker *blockReleaseTracker

	emptyBlocks map[string]string

	// poolManager associates IPPools with their blocks.
	poolManager *poolManager

	// Cache datastoreReady to avoid too much API queries.
	datastoreReady bool

	// Channel for indicating that Kubernetes nodes have been deleted.
	nodeDeletionChan chan struct{}
	podDeletionChan  chan *v1.Pod

	// consolidationWindow is the time to wait for additional updates after receiving one before processing the updates
	// received. This is to allow for multiple node deletion events to be consolidated into a single event.
	consolidationWindow time.Duration

	// For unit testing purposes.
	pauseRequestChannel chan pauseRequest

	// fullSyncRequired marks whether or not a full scan of IPAM data is required on the next sync.
	fullSyncRequired bool

	// retryController manages retries and backoff of full IPAM syncs.
	retryController *utils.RetryController
}

func (c *IPAMController) Start(stop chan struct{}) {
	go c.acceptScheduleRequests(stop)
}

func (c *IPAMController) RegisterWith(f *utils.DataFeed) {
	f.RegisterForNotification(model.BlockKey{}, c.onUpdate)
	f.RegisterForNotification(model.ResourceKey{}, c.onUpdate)
	f.RegisterForSyncStatus(c.onStatusUpdate)
}

func (c *IPAMController) onStatusUpdate(s bapi.SyncStatus) {
	c.syncerUpdates <- s
}

func (c *IPAMController) onUpdate(update bapi.Update) {
	switch update.KVPair.Key.(type) {
	case model.ResourceKey:
		switch update.KVPair.Key.(model.ResourceKey).Kind {
		case libapiv3.KindNode, apiv3.KindIPPool, apiv3.KindClusterInformation:
			c.syncerUpdates <- update.KVPair
		}
	case model.BlockKey:
		c.syncerUpdates <- update.KVPair
	}
}

func (c *IPAMController) OnKubernetesNodeDeleted(n *v1.Node) {
	log.WithField("node", n.Name).Debug("Kubernetes node deletion event")
	c.nodeDeletionChan <- struct{}{}
}

func (c *IPAMController) OnKubernetesPodDeleted(p *v1.Pod) {
	log.WithField("pod", p.Name).Debug("Kubernetes pod deletion event")
	c.podDeletionChan <- p
}

// fullScanNextSync marks the IPAMController for a full resync on the next syncIPAM call.
func (c *IPAMController) fullScanNextSync(reason string) {
	if c.fullSyncRequired {
		log.WithField("reason", reason).Debug("Full resync already pending")
		return
	}
	c.fullSyncRequired = true
	log.WithField("reason", reason).Info("Marking IPAM for full resync")
}

// acceptScheduleRequests is the main worker routine of the IPAM controller. It monitors
// the updates channel and triggers syncs.
func (c *IPAMController) acceptScheduleRequests(stopCh <-chan struct{}) {
	// Periodic sync ticker.
	period := 5 * time.Minute
	if c.config.LeakGracePeriod != nil {
		if c.config.LeakGracePeriod.Duration > 0 {
			period = c.config.LeakGracePeriod.Duration / 2
		}
	}
	t := time.NewTicker(period)
	log.Infof("Will run periodic IPAM sync every %s", period)

	for {
		// Wait until something wakes us up, or we are stopped.
		select {
		case <-c.nodeDeletionChan:
			logEntry := log.WithFields(log.Fields{"controller": "ipam", "type": "nodeDeletion"})
			utils.ProcessBatch(c.nodeDeletionChan, struct{}{}, nil, logEntry)

			// When one or more nodes are deleted, trigger a full sync to ensure that we release
			// their affinities.
			c.fullScanNextSync("Batch node deletion")
			kick(c.syncChan)
		case pod := <-c.podDeletionChan:
			logEntry := log.WithFields(log.Fields{"controller": "ipam", "type": "podDeletion"})
			utils.ProcessBatch(c.podDeletionChan, pod, c.allocationState.markDirtyPodDeleted, logEntry)
			kick(c.syncChan)
		case upd := <-c.syncerUpdates:
			logEntry := log.WithFields(log.Fields{"controller": "ipam", "type": "syncerUpdate"})
			utils.ProcessBatch(c.syncerUpdates, upd, c.handleUpdate, logEntry)
			kick(c.syncChan)
		case <-t.C:
			// Periodic IPAM sync, queue a full scan of the IPAM data.
			c.fullScanNextSync("periodic sync")

			log.Debug("Periodic IPAM sync")
			err := c.syncIPAM()
			if err != nil {
				log.WithError(err).Warn("Periodic IPAM sync failed")
			}
			log.Debug("Periodic IPAM sync complete")
		case <-c.syncChan:
			// Triggered IPAM sync.
			log.Debug("Triggered IPAM sync")
			err := c.syncIPAM()
			if err != nil {
				// For errors, tell the retry controller to schedule a retry. It will ensure at most
				// one retry is queued at a time, and also manage backoff.
				log.WithError(err).Warn("error syncing IPAM data")
				c.retryController.ScheduleRetry()
			} else {
				// Mark sync as a success.
				c.retryController.Success()
			}

			// Update prometheus metrics.
			c.updateMetrics()
			log.Debug("Triggered IPAM sync complete")
		case req := <-c.pauseRequestChannel:
			// For testing purposes - allow the tests to pause the main processing loop.
			log.Warn("Pausing main loop so tests can read state")
			req.pauseConfirmed <- struct{}{}
			<-req.doneChan
		case <-stopCh:
			return
		}
	}
}

// handleUpdate fans out proper handling of the update depending on the
// information in the update.
func (c *IPAMController) handleUpdate(upd interface{}) {
	switch upd := upd.(type) {
	case bapi.SyncStatus:
		c.syncStatus = upd
		switch upd {
		case bapi.InSync:
			log.WithField("status", upd).Info("Syncer is InSync, kicking sync channel")
			kick(c.syncChan)
		}
		return
	case model.KVPair:
		switch upd.Key.(type) {
		case model.ResourceKey:
			switch upd.Key.(model.ResourceKey).Kind {
			case libapiv3.KindNode:
				c.handleNodeUpdate(upd)
				return
			case apiv3.KindIPPool:
				c.handlePoolUpdate(upd)
				return
			case apiv3.KindClusterInformation:
				c.handleClusterInformationUpdate(upd)
				return
			}
		case model.BlockKey:
			c.handleBlockUpdate(upd)
			return
		}
	}
	log.WithField("update", upd).Warn("Unexpected update received")
}

// handleBlockUpdate wraps up the logic to execute when receiving a block update.
func (c *IPAMController) handleBlockUpdate(kvp model.KVPair) {
	if kvp.Value != nil {
		c.onBlockUpdated(kvp)
	} else {
		c.onBlockDeleted(kvp.Key.(model.BlockKey))
	}
}

// handleNodeUpdate wraps up the logic to execute when receiving a node update.
func (c *IPAMController) handleNodeUpdate(kvp model.KVPair) {
	if kvp.Value != nil {
		n := kvp.Value.(*libapiv3.Node)
		kn, err := getK8sNodeName(*n)
		if err != nil {
			log.WithError(err).Info("Unable to get corresponding k8s node name")
		}

		// Maintain mapping of Calico node to Kubernetes node. Ensure all Calico nodes have an entry in the map by
		// assigning a value of "" for nodes that are not orchestrated by Kubernetes.
		if current, ok := c.kubernetesNodesByCalicoName[n.Name]; !ok {
			log.Debugf("Add mapping calico node -> k8s node. %s -> %s", n.Name, kn)
			c.kubernetesNodesByCalicoName[n.Name] = kn
		} else if current != kn {
			log.Warnf("Update mapping calico node -> k8s node. %s -> %s (previously %s)", n.Name, kn, current)
			c.kubernetesNodesByCalicoName[n.Name] = kn
		}
	} else {
		cnode := kvp.Key.(model.ResourceKey).Name
		if _, ok := c.kubernetesNodesByCalicoName[cnode]; ok {
			log.Debugf("Remove mapping for calico node %s", cnode)
			delete(c.kubernetesNodesByCalicoName, cnode)
		}
	}
}

func (c *IPAMController) handlePoolUpdate(kvp model.KVPair) {
	if kvp.Value != nil {
		pool := kvp.Value.(*apiv3.IPPool)
		c.onPoolUpdated(pool)
	} else {
		poolName := kvp.Key.(model.ResourceKey).Name
		c.onPoolDeleted(poolName)
	}
}

// handleClusterInformationUpdate wraps the logic to execute when receiving a clusterinformation update.
func (c *IPAMController) handleClusterInformationUpdate(kvp model.KVPair) {
	if kvp.Value != nil {
		ci := kvp.Value.(*apiv3.ClusterInformation)
		if ci.Spec.DatastoreReady != nil {
			c.datastoreReady = *ci.Spec.DatastoreReady
		}
	} else {
		c.datastoreReady = false
	}
}

func (c *IPAMController) onBlockUpdated(kvp model.KVPair) {
	blockCIDR := kvp.Key.(model.BlockKey).CIDR.String()
	log.WithField("block", blockCIDR).Debug("Received block update")
	b := kvp.Value.(*model.AllocationBlock)

	// Include affinity if it exists. We want to track nodes even
	// if there are no IPs actually assigned to that node so that we can
	// release their affinity if needed.
	var n string
	if b.Affinity != nil {
		if strings.HasPrefix(*b.Affinity, "host:") {
			n = strings.TrimPrefix(*b.Affinity, "host:")
			c.nodesByBlock[blockCIDR] = n
			if _, ok := c.blocksByNode[n]; !ok {
				c.blocksByNode[n] = map[string]bool{}
			}
			c.blocksByNode[n][blockCIDR] = true
		}
	} else {
		// Affinity may have been removed.
		if n, ok := c.nodesByBlock[blockCIDR]; ok {
			delete(c.nodesByBlock, blockCIDR)
			delete(c.blocksByNode[n], blockCIDR)
		}
	}

	// Update allocations contributed from this block.
	numAllocationsInBlock := 0
	currentAllocations := map[string]bool{}
	for ord, idx := range b.Allocations {
		if idx == nil {
			// Not allocated.
			continue
		}
		numAllocationsInBlock++
		attr := b.Attributes[*idx]

		// If there is no handle, then skip this IP. We need the handle
		// in order to release the IP below.
		if attr.AttrPrimary == nil {
			continue
		}
		handle := *attr.AttrPrimary

		alloc := allocation{
			ip:             ordinalToIP(b, ord).String(),
			handle:         handle,
			attrs:          attr.AttrSecondary,
			sequenceNumber: b.GetSequenceNumberForOrdinal(ord),
			block:          blockCIDR,
		}

		currentAllocations[alloc.id()] = true

		// Check if we already know about this allocation.
		if _, ok := c.allocationsByBlock[blockCIDR][alloc.id()]; ok {
			continue
		}

		// This is a new allocation.
		if _, ok := c.allocationsByBlock[blockCIDR]; !ok {
			c.allocationsByBlock[blockCIDR] = map[string]*allocation{}
		}
		c.allocationsByBlock[blockCIDR][alloc.id()] = &alloc

		// Update the allocations-by-node view.
		if node := alloc.node(); node != "" {
			c.allocationState.allocate(&alloc)
		}
		c.handleTracker.setAllocation(&alloc)
		log.WithFields(alloc.fields()).Debug("New IP allocation")
	}

	// Check if the block is empty and schedule it for GC if needed.
	// Skip blocks without an affinity, since these will be cleaned up when their last address is freed and
	// thus we don't need to do any explicit GC.
	delete(c.emptyBlocks, blockCIDR)
	if n != "" && numAllocationsInBlock == 0 {
		// The block is empty and affine to a node - add it to the emptyBlocks map. We'll check these blocks
		// later to see if they can be cleaned up.
		c.emptyBlocks[blockCIDR] = n
	} else if n != "" {
		// The block is assigned to a node and not empty - mark it as in-use, clearing any previous empty
		// status if it had been marked empty before.
		c.blockReleaseTracker.markInUse(blockCIDR)
	}

	// Remove any previously assigned allocations that have since been released.
	for id, alloc := range c.allocationsByBlock[blockCIDR] {
		if _, ok := currentAllocations[id]; !ok {
			// Needs release.
			c.handleTracker.removeAllocation(alloc)
			delete(c.allocationsByBlock[blockCIDR], id)

			// Also remove from the node view.
			node := alloc.node()
			if node != "" {
				c.allocationState.release(alloc)
			}

			// And to be safe, remove from confirmed leaks just in case.
			delete(c.confirmedLeaks, id)
		}
	}

	c.poolManager.onBlockUpdated(blockCIDR)

	// Finally, update the raw storage.
	c.allBlocks[blockCIDR] = kvp
}

func (c *IPAMController) onBlockDeleted(key model.BlockKey) {
	blockCIDR := key.CIDR.String()
	log.WithField("block", blockCIDR).Info("Received block delete")

	// Remove allocations that were contributed by this block.
	allocations := c.allocationsByBlock[blockCIDR]
	for _, alloc := range allocations {
		node := alloc.node()
		if node != "" {
			c.allocationState.release(alloc)
		}
	}
	delete(c.allocationsByBlock, blockCIDR)

	// Remove from raw block storage.
	if n := c.nodesByBlock[blockCIDR]; n != "" {
		// The block was assigned to a node, make sure to update internal cache.
		delete(c.blocksByNode[n], blockCIDR)
	}
	delete(c.allBlocks, blockCIDR)
	delete(c.nodesByBlock, blockCIDR)
	delete(c.emptyBlocks, blockCIDR)

	c.blockReleaseTracker.onBlockDeleted(blockCIDR)
	c.poolManager.onBlockDeleted(blockCIDR)
}

func (c *IPAMController) onPoolUpdated(pool *apiv3.IPPool) {
	if c.poolManager.allPools[pool.Name] == nil {
		registerMetricVectorsForPool(pool.Name)
		publishPoolSizeMetric(pool)
	}

	c.poolManager.onPoolUpdated(pool)
}

func (c *IPAMController) onPoolDeleted(poolName string) {
	unregisterMetricVectorsForPool(poolName)
	clearPoolSizeMetric(poolName)

	c.poolManager.onPoolDeleted(poolName)
}

func (c *IPAMController) updateMetrics() {
	if !c.datastoreReady {
		log.Warn("datastore is locked, skipping metrics sync")
		return
	}

	// Skip if not InSync yet.
	if c.syncStatus != bapi.InSync {
		log.WithField("status", c.syncStatus).Debug("Have not yet received InSync notification, skipping metrics sync.")
		return
	}

	log.Debug("Gathering latest IPAM state for metrics")

	// Keep track of various counts so that we can report them as metrics. These counts track legacy metrics by node.
	legacyBlocksByNode := map[string]int{}
	legacyBorrowedIPsByNode := map[string]int{}

	// Iterate blocks to determine the correct metric values.
	for poolName, poolBlocks := range c.poolManager.blocksByPool {
		// These counts track pool-based gauges by node for the current pool.
		inUseAllocationsByNode := c.createZeroedMapForNodeValues(poolName)
		borrowedAllocationsByNode := c.createZeroedMapForNodeValues(poolName)
		gcCandidatesByNode := c.createZeroedMapForNodeValues(poolName)
		blocksByNode := map[string]int{}

		for blockCIDR := range poolBlocks {
			b := c.allBlocks[blockCIDR].Value.(*model.AllocationBlock)

			affineNode := "no_affinity"
			if b.Affinity != nil && strings.HasPrefix(*b.Affinity, "host:") {
				affineNode = strings.TrimPrefix(*b.Affinity, "host:")
			}

			legacyBlocksByNode[affineNode]++
			blocksByNode[affineNode]++

			// Go through each IPAM allocation, check its attributes for the node it is assigned to.
			for _, allocation := range c.allocationsByBlock[blockCIDR] {
				// Track nodes based on IP allocations.
				allocationNode := allocation.node()
				if allocationNode == "" {
					allocationNode = unknownNodeLabel
				}

				// Update metrics maps with this allocation.
				inUseAllocationsByNode[allocationNode]++

				if allocationNode != unknownNodeLabel && (b.Affinity == nil || allocationNode != affineNode) {
					// If the allocation's node doesn't match the block's, then this is borrowed.
					legacyBorrowedIPsByNode[allocationNode]++
					borrowedAllocationsByNode[allocationNode]++
				}

				// Update candidate count. Include confirmed leaks as well, in case there is an issue keeping them
				// from being immediately reclaimed as usual.
				if allocation.isCandidateLeak() || allocation.isConfirmedLeak() {
					gcCandidatesByNode[allocationNode]++
				}
			}
		}

		// Update gauge values, resetting the values for the current pool
		updatePoolGaugeWithNodeValues(inUseAllocationGauges, poolName, inUseAllocationsByNode)
		updatePoolGaugeWithNodeValues(borrowedAllocationGauges, poolName, borrowedAllocationsByNode)
		updatePoolGaugeWithNodeValues(blocksGauges, poolName, blocksByNode)
		updatePoolGaugeWithNodeValues(gcCandidateGauges, poolName, gcCandidatesByNode)
	}

	// Update legacy gauges
	legacyAllocationsGauge.Reset()
	c.allocationState.iter(func(node string, allocations map[string]*allocation) {
		legacyAllocationsGauge.WithLabelValues(node).Set(float64(len(allocations)))
	})
	legacyBlocksGauge.Reset()
	for node, num := range legacyBlocksByNode {
		legacyBlocksGauge.WithLabelValues(node).Set(float64(num))
	}
	legacyBorrowedGauge.Reset()
	for node, num := range legacyBorrowedIPsByNode {
		legacyBorrowedGauge.WithLabelValues(node).Set(float64(num))
	}
	log.Debug("IPAM metrics updated")
}

// releaseUnusedBlocks looks at known empty blocks, and releases their affinity
// if appropriate. A block is a candidate for having its affinity released if:
//
// - The block is empty.
// - The block's node has at least one other affine block.
// - The node is not currently undergoing a migration from Flannel
//
// A block will only be released if it has been in this state for longer than the configured
// grace period, which defaults to 15m.
func (c *IPAMController) releaseUnusedBlocks() error {
	for blockCIDR, node := range c.emptyBlocks {
		logc := log.WithFields(log.Fields{"blockCIDR": blockCIDR, "node": node})
		nodeBlocks := c.blocksByNode[node]
		if len(nodeBlocks) <= 1 {
			continue
		}

		// During a Flannel migration, we can only migrate blocks affined to nodes that have already undergone the migration
		migrating, err := c.nodeIsBeingMigrated(node)
		if err != nil {
			logc.WithError(err).Warn("Failed to check if node is being migrated from Flannel, skipping affinity release")
			c.blockReleaseTracker.markInUse(blockCIDR)
			continue
		}
		if migrating {
			logc.Info("Node affined to block is currently undergoing a migration from Flannel, skipping affinity release")
			c.blockReleaseTracker.markInUse(blockCIDR)
			continue
		}

		// Block is a candidate for GC. We don't want to release it immediately after it is created, since there is a
		// small but valid window when allocating an IP that can result in an empty block. Make sure this block is empty
		// for the duration of the grace period before deletion.
		if !c.blockReleaseTracker.markEmpty(blockCIDR) {
			logc.Debug("Block is empty, but still within grace period")
			continue
		}

		// Find the actual block object.
		block, ok := c.allBlocks[blockCIDR]
		if !ok {
			logc.Warn("Couldn't find empty block in cache, skipping affinity release")
			continue
		}

		// We can release the empty one.
		logc.Infof("Releasing affinity for empty block (node has %d total blocks)", len(nodeBlocks))
		err = c.client.IPAM().ReleaseBlockAffinity(context.TODO(), block.Value.(*model.AllocationBlock), true)
		if err != nil {
			logc.WithError(err).Warn("unable or unwilling to release affinity for block")
			continue
		}

		// Update internal state. We released affinity on an empty block, and so
		// it will have been deleted. It's important that we update blocksByNode here
		// in case there are other empty blocks allocated to the node so that we don't
		// accidentally release all of the node's blocks.
		delete(c.emptyBlocks, blockCIDR)
		delete(c.blocksByNode[node], blockCIDR)
		delete(c.nodesByBlock, blockCIDR)
		delete(c.allBlocks, blockCIDR)

		c.blockReleaseTracker.onBlockDeleted(blockCIDR)
		c.poolManager.onBlockDeleted(blockCIDR)
	}
	return nil
}

// checkAllocations scans Calico IPAM and determines if any IPs appear to be leaks, and if any nodes should have their
// block affinities released.
//
// An IP allocation is a candidate for GC when:
// - The referenced pod does not exist in the k8s API.
// - The referenced pod exists, but has a mismatched IP.
//
// An IP allocation is confirmed for GC when:
// - It has been a leak candidate for >= the grace period.
// - It is a leak candidate and it's node has been deleted.
//
// A node's affinities should be released when:
// - The node no longer exists in the Kubernetes API, AND
// - There are no longer any IP allocations on the node, OR
// - The remaining IP allocations on the node are all determined to be leaked IP addresses.
func (c *IPAMController) checkAllocations() ([]string, error) {
	defer logIfSlow(time.Now(), "Allocation scan complete")

	// For each node present in IPAM, if it doesn't exist in the Kubernetes API then we
	// should consider it a candidate for cleanup.
	nodesToCheck := map[string]map[string]*allocation{}

	if c.fullSyncRequired {
		// If a full sync is required, we need to consider all nodes in the IPAM cache - not just the ones that
		// have changed since the last sync. This is a more expensive operation, so we only do it periodically.
		log.Info("Performing a full scan of IPAM allocations to check for leaks and redundant affinities")

		for _, node := range c.nodesByBlock {
			// For each affine block, add an entry. This makes sure we consider them even
			// if they have no allocations.
			nodesToCheck[node] = nil
		}

		// Add in allocations for any nodes that have them.
		c.allocationState.iter(func(node string, allocations map[string]*allocation) {
			nodesToCheck[node] = allocations
		})

		// Clear the full sync flag.
		c.fullSyncRequired = false
	} else {
		log.Debug("Checking dirty nodes for leaks and redundant affinities")

		// Collect allocation state for all nodes that have changed since the last sync.
		c.allocationState.iterDirty(func(node string, allocations map[string]*allocation) {
			log.WithField("node", node).Debug("Node is dirty, checking for leaks")
			nodesToCheck[node] = allocations
		})
	}

	// nodesToRelease tracks nodes that exist in Calico IPAM, but do not exist in the Kubernetes API.
	// These nodes should have all of their block affinities released.
	nodesToRelease := []string{}

	for cnode, allocations := range nodesToCheck {
		// Lookup the corresponding Kubernetes node for each Calico node we found in IPAM.
		// In KDD mode, these are identical. However, in etcd mode its possible that the Calico node has a
		// different name from the Kubernetes node.
		// In KDD mode, if the Node has been deleted from the Kubernetes API, this may be an empty string.
		knode, err := c.kubernetesNodeForCalico(cnode)
		if err != nil {
			if _, ok := err.(*ErrorNotKubernetes); !ok {
				log.Debug("Skipping non-kubernetes node")
			} else {
				log.WithError(err).Warnf("Failed to lookup corresponding node, skipping %s", cnode)
			}
			continue
		}
		logc := log.WithFields(log.Fields{"calicoNode": cnode, "k8sNode": knode})

		// If we found a corresponding k8s node name, check to make sure it is gone. If we
		// found no corresponding node, then we're good to clean up any allocations.
		// We'll check each allocation to make sure it comes from Kubernetes (or is a tunnel address)
		// before cleaning it up below.
		kubernetesNodeExists := false
		if knode != "" && c.nodeExists(knode) {
			logc.Debug("Node still exists")
			kubernetesNodeExists = true
		}
		logc.Debug("Checking node")

		// Tunnel addresses are special - they should only be marked as a leak if the node itself
		// is deleted, and there are no other valid allocations on the node. Keep track of them
		// in this slice so we can mark them for GC when we decide if the node should be cleaned up
		// or not.
		tunnelAddresses := []*allocation{}

		// To increase our confidence, go through each IP address and
		// check to see if the pod it references exists. If all the pods on that node are gone,
		// we can delete it. If any pod still exists, we skip this node. We want to be
		// extra sure that the node is gone before we clean it up.
		canDelete := true
		for _, a := range allocations {
			// Set the Kubernetes node field now that we know the kubernetes node name
			// for this allocation.
			a.knode = knode

			logc = log.WithFields(a.fields())
			if a.isWindowsReserved() {
				// Windows reserved IPs don't need garbage collection. They get released automatically when
				// the block is released.
				logc.Debug("Skipping Windows reserved IP address")
				continue
			}

			if !a.isPodIP() && !a.isTunnelAddress() {
				// Skip any allocations which are not either a Kubernetes pod, or a node's
				// IPIP, VXLAN or Wireguard address. In practice, we don't expect these, but they might exist.
				// When they do, they will need to be released outside of this controller in order for
				// the block to be cleaned up.
				logc.Info("IP allocation on node is from an unknown source. Will be unable to cleanup block until it is removed.")
				canDelete = false
				continue
			}

			if a.isTunnelAddress() {
				// Handle tunnel addresses below.
				tunnelAddresses = append(tunnelAddresses, a)
				continue
			}

			if c.allocationIsValid(a, true) {
				// Allocation is still valid. We can't cleanup the node yet, even
				// if it appears to be deleted, because the allocation's validity breaks
				// our confidence.
				canDelete = false
				a.markValid()
				continue
			} else if !kubernetesNodeExists {
				// The allocation is NOT valid, we can skip the candidacy stage.
				// We know this with confidence because:
				// - The node the allocation belongs to no longer exists.
				// - The pod owning this allocation no longer exists.
				a.markConfirmedLeak()
			} else if c.config.LeakGracePeriod != nil {
				// The allocation is NOT valid, but the Kubernetes node still exists, so our confidence is lower.
				// Mark as a candidate leak. If this state remains, it will switch
				// to confirmed after the grace period.
				a.markLeak(c.config.LeakGracePeriod.Duration)
			}

			if a.isConfirmedLeak() {
				// If the address is determined to be a confirmed leak, add it to the index.
				c.confirmedLeaks[a.id()] = a
			} else if _, ok := c.confirmedLeaks[a.id()]; ok {
				// Address used to be a leak, but is no longer.
				logc.Info("Leaked IP has been resurrected")
				delete(c.confirmedLeaks, a.id())
			}
		}

		if !kubernetesNodeExists {
			if !canDelete {
				// There are still valid allocations on the node.
				logc.Infof("Can't cleanup node yet - IPs still in use on this node")
				continue
			}

			// Mark the node's tunnel addresses for GC.
			for _, a := range tunnelAddresses {
				a.markConfirmedLeak()
				c.confirmedLeaks[a.id()] = a
			}

			// The node is ready have its IPAM affinities released. It exists in Calico IPAM, but
			// not in the Kubernetes API. Additionally, we've checked that there are no
			// outstanding valid allocations on the node.
			nodesToRelease = append(nodesToRelease, cnode)
		}
	}
	return nodesToRelease, nil
}

// allocationIsValid returns true if the allocation is still in use, and false if the allocation
// appears to be leaked.
func (c *IPAMController) allocationIsValid(a *allocation, preferCache bool) bool {
	ns := a.attrs[ipam.AttributeNamespace]
	pod := a.attrs[ipam.AttributePod]
	logc := log.WithFields(a.fields())

	if a.isTunnelAddress() {
		// Tunnel addresses are only valid if the hosting node still exists.
		return a.knode != ""
	}

	if ns == "" || pod == "" {
		// Allocation is either not a pod address, or it pre-dates the use of these
		// attributes. Assume it's a valid allocation since we can't perform our
		// confidence checks below.
		logc.Debug("IP allocation is missing metadata, cannot confirm or deny validity. Assume valid.")
		return true
	}

	// Query the pod referenced by this allocation. If preferCache is true, then check the cache first.
	var err error
	var p *v1.Pod
	if preferCache {
		logc.Debug("Checking cache for pod")
		p, err = c.podLister.Pods(ns).Get(pod)
	} else {
		logc.Debug("Querying Kubernetes API for pod")
		p, err = c.clientset.CoreV1().Pods(ns).Get(context.Background(), pod, metav1.GetOptions{})
	}
	if err != nil {
		if !errors.IsNotFound(err) {
			log.WithError(err).Warn("Failed to query pod, assume it exists and allocation is valid")
			return true
		}
		// Pod not found. Assume this is a leak.
		logc.Debug("Pod not found, assume it's a leak")
		return false
	}

	// The pod exists - check if it is still on the original node.
	// TODO: Do we need this check?
	if p.Spec.NodeName != "" && a.knode != "" && p.Spec.NodeName != a.knode {
		// If the pod has been rescheduled to a new node, we can treat the old allocation as
		// gone and clean it up.
		fields := log.Fields{"old": a.knode, "new": p.Spec.NodeName}
		logc.WithFields(fields).Info("Pod rescheduled on new node. Allocation no longer valid")
		return false
	}

	// Check to see if the pod actually has the IP in question. Gate based on the presence of the
	// status field, which is populated by kubelet.
	if p.Status.PodIP == "" || len(p.Status.PodIPs) == 0 {
		// The pod hasn't received an IP yet.
		log.Debugf("Pod IP has not yet been reported, consider allocation valid")
		return true
	}

	// Pod evicted by agent like kubelet, failed forever, safe to release IP resource
	if p.Status.Phase == v1.PodFailed && p.Status.Reason == "Evicted" {
		logc.Debugf("Pod has failed with Evicted. Allocation no longer valid")
		return false
	}

	// Convert the pod to a workload endpoint. This takes advantage of the IP
	// gathering logic already implemented in the converter, and handles exceptional cases like
	// additional WEPs attached to Multus networks.
	conv := conversion.NewConverter()
	kvps, err := conv.PodToWorkloadEndpoints(p)
	if err != nil {
		log.WithError(err).Warn("Failed to parse pod into WEP, consider allocation valid.")
		return true
	}

	for _, kvp := range kvps {
		if kvp == nil || kvp.Value == nil {
			// Shouldn't hit this branch, but better safe than sorry.
			logc.Warn("Pod converted to nil WorkloadEndpoint")
			continue
		}
		wep := kvp.Value.(*libapiv3.WorkloadEndpoint)
		for _, nw := range wep.Spec.IPNetworks {
			ip, _, err := net.ParseCIDR(nw)
			if err != nil {
				logc.WithError(err).Error("Failed to parse WEP IP, assume allocation is valid")
				return true
			}
			allocIP := net.ParseIP(a.ip)
			if allocIP == nil {
				logc.WithField("ip", a.ip).Error("Failed to parse IP, assume allocation is valid")
				return true
			}

			if allocIP.Equal(ip) {
				// Found a match.
				logc.Debugf("Pod has matching IP, allocation is valid")
				return true
			}
		}
	}

	logc.Debugf("Allocated IP no longer in-use by pod")
	return false
}

func (c *IPAMController) syncIPAM() error {
	defer logIfSlow(time.Now(), "IPAM sync complete")

	if !c.datastoreReady {
		log.Warn("datastore is locked, skipping ipam sync")
		return nil
	}

	// Skip if not InSync yet.
	if c.syncStatus != bapi.InSync {
		log.WithField("status", c.syncStatus).Debug("Have not yet received InSync notification, skipping IPAM sync.")
		return nil
	}

	log.Debug("Synchronizing IPAM data")

	// Scan known allocations, determining if there are any IP address leaks
	// or nodes that should have their block affinities released.
	nodesToRelease, err := c.checkAllocations()
	if err != nil {
		return err
	}

	// Release all confirmed leaks. Leaks are confirmed in checkAllocations() above.
	err = c.garbageCollectKnownLeaks()
	if err != nil {
		return err
	}

	// Release any block affinities for empty blocks that are no longer needed.
	// This ensures Nodes don't hold on to blocks that are no longer in use, allowing them to
	// to be claimed elsewhere.
	err = c.releaseUnusedBlocks()
	if err != nil {
		return err
	}

	// Delete any nodes that we determined can be removed in checkAllocations. These
	// nodes are no longer in the Kubernetes API, and have no valid allocations, so can be cleaned up entirely
	// from Calico IPAM.
	if err = c.releaseNodes(nodesToRelease); err != nil {
		return err
	}

	c.allocationState.syncComplete()
	log.Debug("IPAM sync completed")

	// If there is still dirty state, then we need to do another pass.
	if len(c.confirmedLeaks) > 0 {
		log.WithField("num", len(c.confirmedLeaks)).Info("Confirmed leaks still exist, scheduling another pass")
		kick(c.syncChan)
	}
	return nil
}

// garbageCollectKnownLeaks checks all known allocations and garbage collects any confirmed leaks.
func (c *IPAMController) garbageCollectKnownLeaks() error {
	defer logIfSlow(time.Now(), "Leak GC complete")

	// limit the number of concurrent IPs we attempt to release at once.
	maxBatchSize := 10000

	var opts []ipam.ReleaseOptions
	leaks := map[string]*allocation{}
	for id, a := range c.confirmedLeaks {
		logc := log.WithFields(a.fields())

		// Final check that the allocation is leaked. We prefer the cache when the hosting node has been
		// deleted, as we're reasonably confident this is a leak. Otherwise, we go to the API server directly for extra confidence
		// that the Pod is actually gone.
		if c.allocationIsValid(a, a.knode == "") {
			logc.Info("Leaked IP has been resurrected after querying latest state")
			delete(c.confirmedLeaks, id)
			a.markValid()
			continue
		}

		// Ensure that all of the IPs with this handle are in fact leaked.
		if !c.handleTracker.isConfirmedLeak(a.handle) {
			logc.Debug("Some IPs with this handle are still valid, skipping")
			continue
		}

		opts = append(opts, a.ReleaseOptions())
		leaks[a.ReleaseOptions().Address] = a

		if len(opts) >= maxBatchSize {
			break
		}
	}

	if len(opts) == 0 {
		// Nothing to do.
		return nil
	}

	// By releasing multiple IPs at once, we can reduce the number of API calls the underlying IPAM code needs to make
	// in order to release the IPs. This is especially apparent when there are multple IP addresses from the same block
	// that must be released, as they can all be released in a single API call to update the block.
	log.WithField("num", len(opts)).Info("Garbage collecting leaked IP addresses")
	_, releasedOpts, err := c.client.IPAM().ReleaseIPs(context.TODO(), opts...)

	// First, go through the returned options and update allocation state. These are the IPs that were successfully
	// released, or were unallocated to begin with. In either case, we can mark them as released.
	for _, opt := range releasedOpts {
		// Find the allocation that matches these release options.
		a, ok := leaks[opt.Address]
		if !ok {
			log.WithField("opt", opt).Fatalf("BUG: unable to find allocation for release options: %+v", leaks)
		}
		logc := log.WithFields(a.fields())

		// No longer a leak. Remove it here so we're not dependent on receiving
		// the update from the syncer (which we will do eventually, this is just cleaner).
		c.allocationState.release(a)
		c.incrementReclamationMetric(a.block, a.node())
		delete(c.confirmedLeaks, a.id())

		logc.Info("Successfully garbage collected leaked IP address")
		delete(leaks, opt.Address)
	}

	// Note any leaks that we couldn't release.
	for _, a := range leaks {
		logc := log.WithFields(a.fields())
		logc.Warn("Leaked IP address was not successfully garbage collected")
	}

	// Check the error.
	if err != nil {
		if _, ok := err.(cerrors.ErrorResourceDoesNotExist); !ok {
			log.WithError(err).Warn("Failed to garbage collect one or more leaked IP addresses")
			return err
		}
	}
	return nil
}

func (c *IPAMController) releaseNodes(nodes []string) error {
	if len(nodes) == 0 {
		return nil
	}

	log.WithField("num", len(nodes)).Info("Found a batch of nodes to release")
	var storedErr error
	for _, cnode := range nodes {
		logc := log.WithField("node", cnode)

		// Potentially rate limit node cleanup.
		logc.Info("Cleaning up IPAM affinities for deleted node")
		if err := c.cleanupNode(cnode); err != nil {
			// Store the error, but continue. Storing the error ensures we'll retry.
			logc.WithError(err).Warnf("Error cleaning up node")
			storedErr = err
		}
	}
	return storedErr
}

func (c *IPAMController) cleanupNode(cnode string) error {
	// At this point, we've verified that the node isn't in Kubernetes and that all the allocations
	// are tied to pods which don't exist anymore. Clean up any allocations which may still be laying around.
	logc := log.WithField("calicoNode", cnode)

	affinityCfg := ipam.AffinityConfig{
		AffinityType: ipam.AffinityTypeHost,
		Host:         cnode,
	}

	// Release the affinities for this node, requiring that the blocks are empty.
	if err := c.client.IPAM().ReleaseHostAffinities(context.TODO(), affinityCfg, true); err != nil {
		logc.WithError(err).Errorf("Failed to release block affinities for node")
		return err
	}

	clearReclaimedIPCountForNode(cnode)

	logc.Debug("Released all affinities for node")
	return nil
}

// nodeExists returns true if the given node still exists in the Kubernetes API.
func (c *IPAMController) nodeExists(knode string) bool {
	_, err := c.nodeLister.Get(knode)
	if err != nil {
		if errors.IsNotFound(err) {
			return false
		}
		log.WithError(err).Warn("Failed to query node, assume it exists")
	}
	return true
}

// nodeIsBeingMigrated looks up a Kubernetes node for a Calico node and checks,
// if it is marked by the flannel-migration controller to undergo migration.
func (c *IPAMController) nodeIsBeingMigrated(name string) (bool, error) {
	// Find the Kubernetes node referenced by the Calico node
	kname, err := c.kubernetesNodeForCalico(name)
	if err != nil {
		return false, err
	}
	// Get node to inspect labels
	node, err := c.nodeLister.Get(kname)
	if err != nil {
		if errors.IsNotFound(err) { // Node doesn't exist, so isn't being migrated.
			return false, nil
		}
		return false, fmt.Errorf("failed to check node for migration status: %w", err)
	}

	for labelName, labelVal := range node.ObjectMeta.Labels {
		// Check against labels used by the migration controller
		for migrationLabelName, migrationLabelValue := range flannelmigration.NodeNetworkCalico {
			// Only the label value "calico" specifies a migrated node where we can release the affinity
			if labelName == migrationLabelName && labelVal != migrationLabelValue {
				return true, nil
			}
		}
	}

	return false, nil
}

// kubernetesNodeForCalico returns the name of the Kubernetes node that corresponds to this Calico node.
// This function returns an empty string if no corresponding node could be found.
// Returns ErrorNotKubernetes if the given Calico node is not a Kubernetes node.
func (c *IPAMController) kubernetesNodeForCalico(cnode string) (string, error) {
	// Check if we have the node name cached.
	if kn, ok := c.kubernetesNodesByCalicoName[cnode]; ok && kn != "" {
		return kn, nil
	}
	log.WithField("cnode", cnode).Debug("Node not in cache, look it up in the API")

	// If we can't find a matching Kubernetes node, try looking up the Calico node explicitly,
	// since it's theoretically possible the kubernetesNodesByCalicoName is just running behind the actual state of the
	// data store.
	calicoNode, err := c.client.Nodes().Get(context.TODO(), cnode, options.GetOptions{})
	if err != nil {
		if _, ok := err.(cerrors.ErrorResourceDoesNotExist); ok {
			log.WithError(err).Info("Calico Node referenced in IPAM data does not exist")
			return "", nil
		}
		log.WithError(err).Warn("failed to query Calico Node referenced in IPAM data")
		return "", err
	}

	// Try to pull the k8s name from the retrieved Calico node object. If there is no match,
	// this will return an ErrorNotKubernetes, indicating the node should be ignored.
	return getK8sNodeName(*calicoNode)
}

func (c *IPAMController) incrementReclamationMetric(block string, node string) {
	pool := c.poolManager.poolsByBlock[block]
	if node == "" {
		node = unknownNodeLabel
	}
	gcReclamationsCounter := gcReclamationCounters[pool]
	if gcReclamationsCounter == nil {
		log.Warnf("Reclamation count metric vector used for pool %s was not created, skipping publishing", pool)
		return
	}
	gcReclamationsCounter.With(prometheus.Labels{"node": node}).Inc()
}

func registerMetricVectorsForPool(poolName string) {
	inUseAllocationGauges[poolName] = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "ipam_allocations_in_use",
		Help:        "IPs currently allocated in IPAM to a workload or tunnel endpoint.",
		ConstLabels: prometheus.Labels{"ippool": poolName},
	}, []string{"node"})
	prometheus.MustRegister(inUseAllocationGauges[poolName])

	borrowedAllocationGauges[poolName] = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ipam_allocations_borrowed",
		Help: "IPs currently allocated in IPAM to a workload or tunnel endpoint, where the allocation was borrowed " +
			"from a block affine to another node.",
		ConstLabels: prometheus.Labels{"ippool": poolName},
	}, []string{"node"})
	prometheus.MustRegister(borrowedAllocationGauges[poolName])

	blocksGauges[poolName] = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        "ipam_blocks",
		Help:        "IPAM blocks currently allocated for the IP pool.",
		ConstLabels: prometheus.Labels{"ippool": poolName},
	}, []string{"node"})
	prometheus.MustRegister(blocksGauges[poolName])

	gcCandidateGauges[poolName] = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ipam_allocations_gc_candidates",
		Help: "Allocations that are currently marked by the garbage collector as potential candidates to " +
			"reclaim. Under normal operation, this metric should return to zero after the garbage collector " +
			"confirms that this allocation can be reclaimed and reclaims it, or the allocation is confirmed as valid.",
		ConstLabels: prometheus.Labels{"ippool": poolName},
	}, []string{"node"})
	prometheus.MustRegister(gcCandidateGauges[poolName])

	gcReclamationCounters[poolName] = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ipam_allocations_gc_reclamations",
		Help: "The total allocations that have been reclaimed by the garbage collector over time. Under normal " +
			"operation, this counter should increase, and increases of this counter should align to a return to zero " +
			"for the candidate gauge.",
		ConstLabels: prometheus.Labels{"ippool": poolName},
	}, []string{"node"})
	prometheus.MustRegister(gcReclamationCounters[poolName])
}

func unregisterMetricVectorsForPool(poolName string) {
	if _, ok := inUseAllocationGauges[poolName]; ok {
		prometheus.Unregister(inUseAllocationGauges[poolName])
		delete(inUseAllocationGauges, poolName)
	}

	if _, ok := borrowedAllocationGauges[poolName]; ok {
		prometheus.Unregister(borrowedAllocationGauges[poolName])
		delete(borrowedAllocationGauges, poolName)
	}

	if _, ok := blocksGauges[poolName]; ok {
		prometheus.Unregister(blocksGauges[poolName])
		delete(blocksGauges, poolName)
	}

	if _, ok := gcCandidateGauges[poolName]; ok {
		prometheus.Unregister(gcCandidateGauges[poolName])
		delete(gcCandidateGauges, poolName)
	}

	if _, ok := gcReclamationCounters[poolName]; ok {
		prometheus.Unregister(gcReclamationCounters[poolName])
		delete(gcReclamationCounters, poolName)
	}
}

// Creates map used to index gauge values by node, and seeds with zeroes to create explicit zero values rather than
// absence of data for a node. This enables users to construct utilization expressions that return 0 when the numerator
// is zero, rather than no data. If the pool is the 'unknown' pool, the map is not seeded.
func (c *IPAMController) createZeroedMapForNodeValues(poolName string) map[string]int {
	valuesByNode := map[string]int{}

	if poolName != unknownPoolLabel {
		for cnode := range c.kubernetesNodesByCalicoName {
			valuesByNode[cnode] = 0
		}
	}

	return valuesByNode
}

func updatePoolGaugeWithNodeValues(gaugesByPool map[string]*prometheus.GaugeVec, pool string, nodeValues map[string]int) {
	poolGauge := gaugesByPool[pool]
	if poolGauge == nil {
		log.Warnf("Gauge metric vector used for pool %s was not created, skipping publishing", pool)
		return
	}

	poolGauge.Reset()
	for node, value := range nodeValues {
		poolGauge.With(prometheus.Labels{"node": node}).Set(float64(value))
	}
}

func publishPoolSizeMetric(pool *apiv3.IPPool) {
	_, poolNet, err := cnet.ParseCIDR(pool.Spec.CIDR)
	if err != nil {
		log.WithError(err).Warnf("Unable to parse CIDR for IP Pool %s", pool.Name)
		return
	}

	ones, bits := poolNet.Mask.Size()
	poolSize := math.Pow(2, float64(bits-ones))
	poolSizeGauge.With(prometheus.Labels{"ippool": pool.Name}).Set(poolSize)
}

func clearPoolSizeMetric(poolName string) {
	poolSizeGauge.Delete(prometheus.Labels{"ippool": poolName})
}

// When we stop tracking a node, clear counters to prevent accumulation of stale metrics.
func clearReclaimedIPCountForNode(node string) {
	for _, reclamationCounter := range gcReclamationCounters {
		reclamationCounter.Delete(prometheus.Labels{"node": node})
	}
}

func ordinalToIP(b *model.AllocationBlock, ord int) net.IP {
	return b.OrdinalToIP(ord).IP
}

// pauseRequest is used internally for testing.
type pauseRequest struct {
	// pauseConfirmed is sent a signal when the main loop is paused.
	pauseConfirmed chan struct{}

	// doneChan can be used to resume the main loop.
	doneChan chan struct{}
}

// pause pauses the controller's main loop until the returned function is called.
// this function is for TESTING PURPOSES ONLY, allowing the tests to safely access
// the controller's data caches without races.
func (c *IPAMController) pause() func() {
	doneChan := make(chan struct{})
	pauseConfirmed := make(chan struct{})
	c.pauseRequestChannel <- pauseRequest{doneChan: doneChan, pauseConfirmed: pauseConfirmed}
	<-pauseConfirmed
	return func() {
		doneChan <- struct{}{}
	}
}

func logIfSlow(start time.Time, msg string) {
	if dur := time.Since(start); dur > 5*time.Second {
		log.WithField("duration", dur).Info(msg)
	}
}
