// package kbucket implements a kademlia 'k-bucket' routing table.
package kbucket

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/peerstore"

	logging "github.com/ipfs/go-log"
)

var log = logging.Logger("table")

var ErrPeerRejectedHighLatency = errors.New("peer rejected; latency too high")
var ErrPeerRejectedNoCapacity = errors.New("peer rejected; insufficient capacity")

// PeerPingFnc is the signature of a function that pings a peer in the Routing Table to determine it's liveliness.
type PeerPingFnc func(ctx context.Context, p peer.ID) error

// PeerConnectednessFnc simply checks if we are connected to a peer or not
type PeerConnectednessFnc func(p peer.ID) bool

// RoutingTable defines the routing table.
type RoutingTable struct {
	// the routing table context
	ctx context.Context
	// function to cancel the RT context
	ctxCancel context.CancelFunc

	// ID of the local peer
	local ID

	// Blanket lock, refine later for better performance
	tabLock sync.RWMutex

	// latency metrics
	metrics peerstore.Metrics

	// Maximum acceptable latency for peers in this cluster
	maxLatency time.Duration

	// kBuckets define all the fingers to other nodes.
	buckets    []*bucket
	bucketsize int

	cplRefreshLk   sync.RWMutex
	cplRefreshedAt map[uint]time.Time

	// notification functions
	PeerRemoved func(peer.ID)
	PeerAdded   func(peer.ID)

	// maxLastSuccessfulOutboundThreshold is the max threshold/upper limit for the value of "lastSuccessfulOutboundQuery"
	// of the peer in the bucket above which we will evict it to make place for a new peer if the bucket
	// is full
	maxLastSuccessfulOutboundThreshold float64
	// the interval between two runs of the Routing table refresh by the DHT.
	rtRefreshInterval    time.Duration
	peerPingFnc          PeerPingFnc
	peerConnectednessFnc PeerConnectednessFnc
}

// NewRoutingTable creates a new routing table with a given bucketsize, local ID, and latency tolerance.
func NewRoutingTable(bucketsize int, localID ID, latency time.Duration, m peerstore.Metrics, maxLastSuccessfulOutboundThreshold float64,
	rtRefreshInterval time.Duration, peerPingFnc PeerPingFnc, peerConnectednessFnc PeerConnectednessFnc) (*RoutingTable, error) {
	rt := &RoutingTable{
		buckets:    []*bucket{newBucket()},
		bucketsize: bucketsize,
		local:      localID,

		maxLatency: latency,
		metrics:    m,

		cplRefreshedAt: make(map[uint]time.Time),

		PeerRemoved: func(peer.ID) {},
		PeerAdded:   func(peer.ID) {},

		maxLastSuccessfulOutboundThreshold: maxLastSuccessfulOutboundThreshold,
		rtRefreshInterval:                  rtRefreshInterval,
	}

	if peerPingFnc == nil {
		return nil, errors.New("peerPing func is nil")
	}
	rt.peerPingFnc = peerPingFnc

	if peerConnectednessFnc == nil {
		return nil, errors.New("peerConnectednesss func is nil")
	}
	rt.peerConnectednessFnc = peerConnectednessFnc

	rt.ctx, rt.ctxCancel = context.WithCancel(context.Background())
	go rt.background()

	return rt, nil
}

func (rt *RoutingTable) background() {
	tickr := time.NewTicker(rt.rtRefreshInterval / 3)
	defer tickr.Stop()

	for {
		select {
		case <-tickr.C:
			// get all peers in the routing table
			rt.tabLock.RLock()
			var peers []PeerInfo
			for _, b := range rt.buckets {
				peers = append(peers, b.peers()...)
			}
			rt.tabLock.RUnlock()

			// start going through them
			for _, ps := range peers {
				// ping the peer if it's due for a ping and evict it if the ping fails
				if time.Since(ps.lastSuccessfulOutboundQuery) > (rt.rtRefreshInterval / 3) {
					livelinessCtx, cancel := context.WithTimeout(rt.ctx, 10*time.Second)
					if err := rt.peerPingFnc(livelinessCtx, ps.Id); err != nil {
						log.Debugf("failed to ping peer=%s, got error=%s, evicting it from the RT", ps.Id, err)
						// below lock and connectedness check helps prevents the following race:
						// we ping a peer and realize it's disconnected -> we somehow get a connection to the peer but after
						// our ping and before we remove it from the RT -> which means rt.TryAdd() will not do anything ->
						// we remove it from the RT thus losing a connected peer.
						rt.tabLock.Lock()
						if !rt.peerConnectednessFnc(ps.Id) {
							rt.removePeer(ps.Id)
						}
						rt.tabLock.Unlock()
					}
					cancel()
				}
			}

		case <-rt.ctx.Done():
			return
		}
	}
}

// Close shuts down the Routing Table & all associated processes.
// It is safe to call this multiple times.
func (rt *RoutingTable) Close() error {
	rt.ctxCancel()
	return nil
}

// TryAddPeer tries to add a peer to the Routing table. If the peer ALREADY exists in the Routing Table, this call is a no-op.
// If the peer is a queryPeer i.e. we queried it or it queried us, we set the lastSuccessfulOutboundQuery to the current time.
// If the peer is just a peer that we connect to/it connected to us without any DHT query, we consider it as having
// no lastSuccessfulOutboundQuery.
//
// If the logical bucket to which the peer belongs is full and it's not the last bucket, we try to replace an existing peer
// whose lastSuccessfulOutboundQuery is above the maximum allowed threshold in that bucket with the new peer.
// If no such peer exists in that bucket, we do NOT add the peer to the Routing Table and return error "ErrPeerRejectedNoCapacity".

// It returns a boolean value set to true if the peer was newly added to the Routing Table, false otherwise.
// It also returns any error that occurred while adding the peer to the Routing Table. If the error is not nil,
// the boolean value will ALWAYS be false i.e. the peer wont be added to the Routing Table it it's not already there.
//
// A return value of false with error=nil indicates that the peer ALREADY exists in the Routing Table.
func (rt *RoutingTable) TryAddPeer(p peer.ID, queryPeer bool) (bool, error) {
	rt.tabLock.Lock()
	defer rt.tabLock.Unlock()

	return rt.addPeer(p, queryPeer)
}

// locking is the responsibility of the caller
func (rt *RoutingTable) addPeer(p peer.ID, queryPeer bool) (bool, error) {
	bucketID := rt.bucketIdForPeer(p)
	bucket := rt.buckets[bucketID]
	var lastSuccessfulOutboundQuery time.Time
	if queryPeer {
		lastSuccessfulOutboundQuery = time.Now()
	}

	// peer already exists in the Routing Table.
	if peer := bucket.getPeer(p); peer != nil {
		return false, nil
	}

	// peer's latency threshold is NOT acceptable
	if rt.metrics.LatencyEWMA(p) > rt.maxLatency {
		// Connection doesnt meet requirements, skip!
		return false, ErrPeerRejectedHighLatency
	}

	// We have enough space in the bucket (whether spawned or grouped).
	if bucket.len() < rt.bucketsize {
		bucket.pushFront(&peerInfo{p, lastSuccessfulOutboundQuery, ConvertPeerID(p)})
		rt.PeerAdded(p)
		return true, nil
	}

	if bucketID == len(rt.buckets)-1 {
		// if the bucket is too large and this is the last bucket (i.e. wildcard), unfold it.
		rt.nextBucket()
		// the structure of the table has changed, so let's recheck if the peer now has a dedicated bucket.
		bucketID = rt.bucketIdForPeer(p)
		bucket = rt.buckets[bucketID]

		// push the peer only if the bucket isn't overflowing after slitting
		if bucket.len() < rt.bucketsize {
			bucket.pushFront(&peerInfo{p, lastSuccessfulOutboundQuery, ConvertPeerID(p)})
			rt.PeerAdded(p)
			return true, nil
		}
	}

	// the bucket to which the peer belongs is full. Let's try to find a peer
	// in that bucket with a lastSuccessfulOutboundQuery value above the maximum threshold and replace it.
	allPeers := bucket.peers()
	for _, pc := range allPeers {
		if float64(time.Since(pc.lastSuccessfulOutboundQuery)) > rt.maxLastSuccessfulOutboundThreshold {
			// let's evict it and add the new peer
			if bucket.remove(pc.Id) {
				bucket.pushFront(&peerInfo{p, lastSuccessfulOutboundQuery, ConvertPeerID(p)})
				rt.PeerAdded(p)
				return true, nil
			}
		}
	}

	return false, ErrPeerRejectedNoCapacity
}

// UpdateLastSuccessfulOutboundQuery updates the lastSuccessfulOutboundQuery time of the peer
// Returns true if the update was successful, false otherwise.
func (rt *RoutingTable) UpdateLastSuccessfulOutboundQuery(p peer.ID, t time.Time) bool {
	rt.tabLock.Lock()
	defer rt.tabLock.Unlock()

	bucketID := rt.bucketIdForPeer(p)
	bucket := rt.buckets[bucketID]

	if pc := bucket.getPeer(p); pc != nil {
		pc.lastSuccessfulOutboundQuery = t
		return true
	}
	return false
}

// RemovePeer should be called when the caller is sure that a peer is not useful for queries.
// For eg: the peer could have stopped supporting the DHT protocol.
// It evicts the peer from the Routing Table.
func (rt *RoutingTable) RemovePeer(p peer.ID) {
	rt.tabLock.Lock()
	defer rt.tabLock.Unlock()
	rt.removePeer(p)
}

// locking is the responsibility of the caller
func (rt *RoutingTable) removePeer(p peer.ID) {
	bucketID := rt.bucketIdForPeer(p)
	bucket := rt.buckets[bucketID]
	if bucket.remove(p) {
		// peer removed callback
		rt.PeerRemoved(p)
		return
	}
}

func (rt *RoutingTable) nextBucket() {
	// This is the last bucket, which allegedly is a mixed bag containing peers not belonging in dedicated (unfolded) buckets.
	// _allegedly_ is used here to denote that *all* peers in the last bucket might feasibly belong to another bucket.
	// This could happen if e.g. we've unfolded 4 buckets, and all peers in folded bucket 5 really belong in bucket 8.
	bucket := rt.buckets[len(rt.buckets)-1]
	newBucket := bucket.split(len(rt.buckets)-1, rt.local)
	rt.buckets = append(rt.buckets, newBucket)

	// The newly formed bucket still contains too many peers. We probably just unfolded a empty bucket.
	if newBucket.len() >= rt.bucketsize {
		// Keep unfolding the table until the last bucket is not overflowing.
		rt.nextBucket()
	}
}

// Find a specific peer by ID or return nil
func (rt *RoutingTable) Find(id peer.ID) peer.ID {
	srch := rt.NearestPeers(ConvertPeerID(id), 1)
	if len(srch) == 0 || srch[0] != id {
		return ""
	}
	return srch[0]
}

// NearestPeer returns a single peer that is nearest to the given ID
func (rt *RoutingTable) NearestPeer(id ID) peer.ID {
	peers := rt.NearestPeers(id, 1)
	if len(peers) > 0 {
		return peers[0]
	}

	log.Debugf("NearestPeer: Returning nil, table size = %d", rt.Size())
	return ""
}

// NearestPeers returns a list of the 'count' closest peers to the given ID
func (rt *RoutingTable) NearestPeers(id ID, count int) []peer.ID {
	// This is the number of bits _we_ share with the key. All peers in this
	// bucket share cpl bits with us and will therefore share at least cpl+1
	// bits with the given key. +1 because both the target and all peers in
	// this bucket differ from us in the cpl bit.
	cpl := CommonPrefixLen(id, rt.local)

	// It's assumed that this also protects the buckets.
	rt.tabLock.RLock()

	// Get bucket index or last bucket
	if cpl >= len(rt.buckets) {
		cpl = len(rt.buckets) - 1
	}

	pds := peerDistanceSorter{
		peers:  make([]peerDistance, 0, count+rt.bucketsize),
		target: id,
	}

	// Add peers from the target bucket (cpl+1 shared bits).
	pds.appendPeersFromList(rt.buckets[cpl].list)

	// If we're short, add peers from buckets to the right until we have
	// enough. All buckets to the right share exactly cpl bits (as opposed
	// to the cpl+1 bits shared by the peers in the cpl bucket).
	//
	// Unfortunately, to be completely correct, we can't just take from
	// buckets until we have enough peers because peers because _all_ of
	// these peers will be ~2**(256-cpl) from us.
	//
	// However, we're going to do that anyways as it's "good enough"

	for i := cpl + 1; i < len(rt.buckets) && pds.Len() < count; i++ {
		pds.appendPeersFromList(rt.buckets[i].list)
	}

	// If we're still short, add in buckets that share _fewer_ bits. We can
	// do this bucket by bucket because each bucket will share 1 fewer bit
	// than the last.
	//
	// * bucket cpl-1: cpl-1 shared bits.
	// * bucket cpl-2: cpl-2 shared bits.
	// ...
	for i := cpl - 1; i >= 0 && pds.Len() < count; i-- {
		pds.appendPeersFromList(rt.buckets[i].list)
	}
	rt.tabLock.RUnlock()

	// Sort by distance to local peer
	pds.sort()

	if count < pds.Len() {
		pds.peers = pds.peers[:count]
	}

	out := make([]peer.ID, 0, pds.Len())
	for _, p := range pds.peers {
		out = append(out, p.p)
	}

	return out
}

// Size returns the total number of peers in the routing table
func (rt *RoutingTable) Size() int {
	var tot int
	rt.tabLock.RLock()
	for _, buck := range rt.buckets {
		tot += buck.len()
	}
	rt.tabLock.RUnlock()
	return tot
}

// ListPeers takes a RoutingTable and returns a list of all peers from all buckets in the table.
func (rt *RoutingTable) ListPeers() []peer.ID {
	rt.tabLock.RLock()
	defer rt.tabLock.RUnlock()

	var peers []peer.ID
	for _, buck := range rt.buckets {
		peers = append(peers, buck.peerIds()...)
	}
	return peers
}

// Print prints a descriptive statement about the provided RoutingTable
func (rt *RoutingTable) Print() {
	fmt.Printf("Routing Table, bs = %d, Max latency = %d\n", rt.bucketsize, rt.maxLatency)
	rt.tabLock.RLock()

	for i, b := range rt.buckets {
		fmt.Printf("\tbucket: %d\n", i)

		for e := b.list.Front(); e != nil; e = e.Next() {
			p := e.Value.(*peerInfo).Id
			fmt.Printf("\t\t- %s %s\n", p.Pretty(), rt.metrics.LatencyEWMA(p).String())
		}
	}
	rt.tabLock.RUnlock()
}

// the caller is responsible for the locking
func (rt *RoutingTable) bucketIdForPeer(p peer.ID) int {
	peerID := ConvertPeerID(p)
	cpl := CommonPrefixLen(peerID, rt.local)
	bucketID := cpl
	if bucketID >= len(rt.buckets) {
		bucketID = len(rt.buckets) - 1
	}
	return bucketID
}
