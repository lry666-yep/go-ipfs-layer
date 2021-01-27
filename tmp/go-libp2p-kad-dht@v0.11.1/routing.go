package dht

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"sync"
	"time"
	"encoding/json"

	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/peerstore"
	"github.com/libp2p/go-libp2p-core/routing"

	"github.com/ipfs/go-cid"
	u "github.com/ipfs/go-ipfs-util"
	pb "github.com/libp2p/go-libp2p-kad-dht/pb"
	"github.com/libp2p/go-libp2p-kad-dht/qpeerset"
	kb "github.com/libp2p/go-libp2p-kbucket"
	record "github.com/libp2p/go-libp2p-record"
	"github.com/multiformats/go-multihash"
)

// This file implements the Routing interface for the IpfsDHT struct.

// Basic Put/Get

// PutValue adds value corresponding to given Key.
// This is the top level "Store" operation of the DHT
func (dht *IpfsDHT) PutValue(ctx context.Context, key string, value []byte, opts ...routing.Option) (err error) {
	if !dht.enableValues {
		return routing.ErrNotSupported
	}

	logger.Debugw("putting value", "key", loggableRecordKeyString(key))

	// don't even allow local users to put bad values.
	if err := dht.Validator.Validate(key, value); err != nil {
		return err
	}

	old, err := dht.getLocal(key)
	if err != nil {
		// Means something is wrong with the datastore.
		return err
	}

	// Check if we have an old value that's not the same as the new one.
	if old != nil && !bytes.Equal(old.GetValue(), value) {
		// Check to see if the new one is better.
		i, err := dht.Validator.Select(key, [][]byte{value, old.GetValue()})
		if err != nil {
			return err
		}
		if i != 0 {
			return fmt.Errorf("can't replace a newer value with an older value")
		}
	}

	rec := record.MakePutRecord(key, value)
	rec.TimeReceived = u.FormatRFC3339(time.Now())
	err = dht.putLocal(key, rec)
	if err != nil {
		return err
	}

	pchan, err := dht.GetClosestPeers(ctx, key)
	if err != nil {
		return err
	}

	wg := sync.WaitGroup{}
	for p := range pchan {
		wg.Add(1)
		go func(p peer.ID) {
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			defer wg.Done()
			routing.PublishQueryEvent(ctx, &routing.QueryEvent{
				Type: routing.Value,
				ID:   p,
			})

			err := dht.putValueToPeer(ctx, p, rec)
			if err != nil {
				logger.Debugf("failed putting value to peer: %s", err)
			}
		}(p)
	}
	wg.Wait()

	return nil
}

// RecvdVal stores a value and the peer from which we got the value.
type RecvdVal struct {
	Val  []byte
	From peer.ID
}

// GetValue searches for the value corresponding to given Key.
func (dht *IpfsDHT) GetValue(ctx context.Context, key string, opts ...routing.Option) (_ []byte, err error) {
	if !dht.enableValues {
		return nil, routing.ErrNotSupported
	}

	// apply defaultQuorum if relevant
	var cfg routing.Options
	if err := cfg.Apply(opts...); err != nil {
		return nil, err
	}
	opts = append(opts, Quorum(getQuorum(&cfg, defaultQuorum)))

	responses, err := dht.SearchValue(ctx, key, opts...)
	if err != nil {
		return nil, err
	}
	var best []byte

	for r := range responses {
		best = r
	}

	if ctx.Err() != nil {
		return best, ctx.Err()
	}

	if best == nil {
		return nil, routing.ErrNotFound
	}
	logger.Debugf("GetValue %v %x", loggableRecordKeyString(key), best)
	return best, nil
}

// SearchValue searches for the value corresponding to given Key and streams the results.
func (dht *IpfsDHT) SearchValue(ctx context.Context, key string, opts ...routing.Option) (<-chan []byte, error) {
	if !dht.enableValues {
		return nil, routing.ErrNotSupported
	}

	var cfg routing.Options
	if err := cfg.Apply(opts...); err != nil {
		return nil, err
	}

	responsesNeeded := 0
	if !cfg.Offline {
		responsesNeeded = getQuorum(&cfg, defaultQuorum)
	}

	stopCh := make(chan struct{})
	valCh, lookupRes := dht.getValues(ctx, key, stopCh)

	out := make(chan []byte)
	go func() {
		defer close(out)
		best, peersWithBest, aborted := dht.searchValueQuorum(ctx, key, valCh, stopCh, out, responsesNeeded)
		if best == nil || aborted {
			return
		}

		updatePeers := make([]peer.ID, 0, dht.bucketSize)
		select {
		case l := <-lookupRes:
			if l == nil {
				return
			}

			for _, p := range l.peers {
				if _, ok := peersWithBest[p]; !ok {
					updatePeers = append(updatePeers, p)
				}
			}
		case <-ctx.Done():
			return
		}

		dht.updatePeerValues(dht.Context(), key, best, updatePeers)
	}()

	return out, nil
}

func (dht *IpfsDHT) searchValueQuorum(ctx context.Context, key string, valCh <-chan RecvdVal, stopCh chan struct{},
	out chan<- []byte, nvals int) ([]byte, map[peer.ID]struct{}, bool) {
	numResponses := 0
	return dht.processValues(ctx, key, valCh,
		func(ctx context.Context, v RecvdVal, better bool) bool {
			numResponses++
			if better {
				select {
				case out <- v.Val:
				case <-ctx.Done():
					return false
				}
			}

			if nvals > 0 && numResponses > nvals {
				close(stopCh)
				return true
			}
			return false
		})
}

// GetValues gets nvals values corresponding to the given key.
func (dht *IpfsDHT) GetValues(ctx context.Context, key string, nvals int) (_ []RecvdVal, err error) {
	if !dht.enableValues {
		return nil, routing.ErrNotSupported
	}

	queryCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	valCh, _ := dht.getValues(queryCtx, key, nil)

	out := make([]RecvdVal, 0, nvals)
	for val := range valCh {
		out = append(out, val)
		if len(out) == nvals {
			cancel()
		}
	}

	return out, ctx.Err()
}

func (dht *IpfsDHT) processValues(ctx context.Context, key string, vals <-chan RecvdVal,
	newVal func(ctx context.Context, v RecvdVal, better bool) bool) (best []byte, peersWithBest map[peer.ID]struct{}, aborted bool) {
loop:
	for {
		if aborted {
			return
		}

		select {
		case v, ok := <-vals:
			if !ok {
				break loop
			}

			// Select best value
			if best != nil {
				if bytes.Equal(best, v.Val) {
					peersWithBest[v.From] = struct{}{}
					aborted = newVal(ctx, v, false)
					continue
				}
				sel, err := dht.Validator.Select(key, [][]byte{best, v.Val})
				if err != nil {
					logger.Warnw("failed to select best value", "key", loggableRecordKeyString(key), "error", err)
					continue
				}
				if sel != 1 {
					aborted = newVal(ctx, v, false)
					continue
				}
			}
			peersWithBest = make(map[peer.ID]struct{})
			peersWithBest[v.From] = struct{}{}
			best = v.Val
			aborted = newVal(ctx, v, true)
		case <-ctx.Done():
			return
		}
	}

	return
}

func (dht *IpfsDHT) updatePeerValues(ctx context.Context, key string, val []byte, peers []peer.ID) {
	fixupRec := record.MakePutRecord(key, val)
	for _, p := range peers {
		go func(p peer.ID) {
			//TODO: Is this possible?
			if p == dht.self {
				err := dht.putLocal(key, fixupRec)
				if err != nil {
					logger.Error("Error correcting local dht entry:", err)
				}
				return
			}
			ctx, cancel := context.WithTimeout(ctx, time.Second*30)
			defer cancel()
			err := dht.putValueToPeer(ctx, p, fixupRec)
			if err != nil {
				logger.Debug("Error correcting DHT entry: ", err)
			}
		}(p)
	}
}

func (dht *IpfsDHT) getValues(ctx context.Context, key string, stopQuery chan struct{}) (<-chan RecvdVal, <-chan *lookupWithFollowupResult) {
	valCh := make(chan RecvdVal, 1)
	lookupResCh := make(chan *lookupWithFollowupResult, 1)

	logger.Debugw("finding value", "key", loggableRecordKeyString(key))

	if rec, err := dht.getLocal(key); rec != nil && err == nil {
		select {
		case valCh <- RecvdVal{
			Val:  rec.GetValue(),
			From: dht.self,
		}:
		case <-ctx.Done():
		}
	}

	go func() {
		defer close(valCh)
		defer close(lookupResCh)
		lookupRes, err := dht.runLookupWithFollowup(ctx, key,
			func(ctx context.Context, p peer.ID) ([]*peer.AddrInfo, error) {
				// For DHT query command
				routing.PublishQueryEvent(ctx, &routing.QueryEvent{
					Type: routing.SendingQuery,
					ID:   p,
				})

				rec, peers, err := dht.getValueOrPeers(ctx, p, key)
				switch err {
				case routing.ErrNotFound:
					// in this case, they responded with nothing,
					// still send a notification so listeners can know the
					// request has completed 'successfully'
					routing.PublishQueryEvent(ctx, &routing.QueryEvent{
						Type: routing.PeerResponse,
						ID:   p,
					})
					return nil, err
				default:
					return nil, err
				case nil, errInvalidRecord:
					// in either of these cases, we want to keep going
				}

				// TODO: What should happen if the record is invalid?
				// Pre-existing code counted it towards the quorum, but should it?
				if rec != nil && rec.GetValue() != nil {
					rv := RecvdVal{
						Val:  rec.GetValue(),
						From: p,
					}

					select {
					case valCh <- rv:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}

				// For DHT query command
				routing.PublishQueryEvent(ctx, &routing.QueryEvent{
					Type:      routing.PeerResponse,
					ID:        p,
					Responses: peers,
				})

				return peers, err
			},
			func() bool {
				select {
				case <-stopQuery:
					return true
				default:
					return false
				}
			},
		)

		if err != nil {
			return
		}
		lookupResCh <- lookupRes

		if ctx.Err() == nil {
			dht.refreshRTIfNoShortcut(kb.ConvertKey(key), lookupRes)
		}
	}()

	return valCh, lookupResCh
}

func (dht *IpfsDHT) refreshRTIfNoShortcut(key kb.ID, lookupRes *lookupWithFollowupResult) {
	if lookupRes.completed {
		// refresh the cpl for this key as the query was successful
		dht.routingTable.ResetCplRefreshedAtForID(key, time.Now())
	}
}

// Provider abstraction for indirect stores.
// Some DHTs store values directly, while an indirect store stores pointers to
// locations of the value, similarly to Coral and Mainline DHT.

// Provide makes this node announce that it can provide a value for the given key
func (dht *IpfsDHT) Provide(ctx context.Context, key cid.Cid, brdcst bool) (err error) {
	if !dht.prov {
		//fmt.Println("@lry: prov is false cant Provide")
		return nil
	}
	if !dht.enableProviders {
		return routing.ErrNotSupported
	} else if !key.Defined() {
		return fmt.Errorf("invalid cid: undefined")
	}
	keyMH := key.Hash()
	logger.Debugw("providing", "cid", key, "mh", loggableProviderRecordBytes(keyMH))

	// add self locally
	dht.ProviderManager.AddProvider(ctx, keyMH, dht.self)
	if !brdcst {
		return nil
	}

	closerCtx := ctx
	if deadline, ok := ctx.Deadline(); ok {
		now := time.Now()
		timeout := deadline.Sub(now)

		if timeout < 0 {
			// timed out
			return context.DeadlineExceeded
		} else if timeout < 10*time.Second {
			// Reserve 10% for the final put.
			deadline = deadline.Add(-timeout / 10)
		} else {
			// Otherwise, reserve a second (we'll already be
			// connected so this should be fast).
			deadline = deadline.Add(-time.Second)
		}
		var cancel context.CancelFunc
		closerCtx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}

	var exceededDeadline bool
	peers, err := dht.GetClosestPeers(closerCtx, string(keyMH))
	switch err {
	case context.DeadlineExceeded:
		// If the _inner_ deadline has been exceeded but the _outer_
		// context is still fine, provide the value to the closest peers
		// we managed to find, even if they're not the _actual_ closest peers.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		exceededDeadline = true
	case nil:
	default:
		return err
	}

	mes, err := dht.makeProvRecord(keyMH)
	if err != nil {
		return err
	}

	wg := sync.WaitGroup{}
	count := 0
	for p := range peers {
		fmt.Printf("@lry: closePeer result : %s \n", p)
		if count == 0 {
			wg.Add(1)
			go func(p peer.ID) {
				defer wg.Done()
				logger.Debugf("putProvider(%s, %s)", loggableProviderRecordBytes(keyMH), p)

				fmt.Printf("@lry: putProv  key = %s  ,  to peer = %s \n",  key.String(),  p)
				err := dht.sendMessage(ctx, p, mes)
				if err != nil {
					logger.Debug(err)
				}
			}(p)
		}
		count++
	}
	wg.Wait()
	if exceededDeadline {
		return context.DeadlineExceeded
	}
	return ctx.Err()
}
func (dht *IpfsDHT) makeProvRecord(key []byte) (*pb.Message, error) {
	pi := peer.AddrInfo{
		ID:    dht.self,
		Addrs: dht.host.Addrs(),
	}

	// // only share WAN-friendly addresses ??
	// pi.Addrs = addrutil.WANShareableAddrs(pi.Addrs)
	if len(pi.Addrs) < 1 {
		return nil, fmt.Errorf("no known addresses for self, cannot put provider")
	}

	pmes := pb.NewMessage(pb.Message_ADD_PROVIDER, key, 0)
	pmes.ProviderPeers = pb.RawPeerInfosToPBPeers([]peer.AddrInfo{pi})
	return pmes, nil
}

// FindProviders searches until the context expires.
func (dht *IpfsDHT) FindProviders(ctx context.Context, c cid.Cid) ([]peer.AddrInfo, error) {

	if !dht.enableProviders {
		return nil, routing.ErrNotSupported
	} else if !c.Defined() {
		return nil, fmt.Errorf("invalid cid: undefined")
	}

	var providers []peer.AddrInfo
	for p := range dht.FindProvidersAsync(ctx, c, dht.bucketSize) {
		providers = append(providers, p)
	}
	return providers, nil
}



// 用于实验测量跳数与时延的函数，自己实现的Dht命令调用
func (dht *IpfsDHT) FindProvidersForTest(ctx context.Context, c cid.Cid) (bool, int, float32) {
	fmt.Printf("@lry: FindProvidersForTest key = %s \n", c)

	fmt.Printf("@lry For test jump and Delay \n")
	var providers []peer.AddrInfo
	dht.jump = 0
	preTime := time.Now().UnixNano()

	//一直等到channel被关闭，channel关闭也就是findProv迭代结束
	for p := range dht.FindProvidersAsyncForTest(ctx, c, dht.bucketSize) {
		providers = append(providers, p)
	}

	endTime := time.Now().UnixNano()
	totalTime := endTime - preTime
	fmt.Printf("***@lry_result find Prov len = %d, 【jump = %d】 【time = %f ms】 *** \n", len(providers), dht.jump, (float32)(totalTime/1000000))

	// 数据中心搜索
	if len(providers) > 0 {
		return false, dht.jump, (float32)(totalTime / 1000000)
	}

	if dht.enableDataCenter {
		fmt.Printf("@lry: request to DataCenter, ip=%s\n", dht.dataCenterIP)
		if dht.dataCenterIP == "" {
			fmt.Printf("@lry: DataCenter ip is nil\n")
			return false, dht.jump, (float32)(totalTime / 1000000)
		}
		//HTTP 查找
		urlStr := "http://" + dht.dataCenterIP + ":5001/api/v0/dht/provt?arg=" + c.String()
		fmt.Printf("@lry: sent datastore url %s \n", urlStr)
		resp, err := http.Post(urlStr, "application/json", nil)
		endTime := time.Now().UnixNano()
		totalTime := endTime - preTime
		if err != nil {
			fmt.Println(err)
			return len(providers) == 0, dht.jump, (float32)(totalTime / 1000000)
		}
		defer resp.Body.Close()
		bodyC, _ := ioutil.ReadAll(resp.Body)

		var jsonMap map[string]interface{}
		err = json.Unmarshal(bodyC, &jsonMap)
		if err != nil {
			fmt.Println(err)
			return len(providers) == 0, dht.jump, (float32)(totalTime / 1000000)
		}
		return jsonMap["Ok"].(bool), dht.jump + jsonMap["Jump"].(int), (float32)(totalTime / 1000000)
	}
	//查找失败
	return true, dht.jump, (float32)(totalTime / 1000000)

}

func (dht *IpfsDHT) FindProvidersAsyncForTest(ctx context.Context, key cid.Cid, count int) <-chan peer.AddrInfo {
	if !dht.enableProviders || !key.Defined() {
		peerOut := make(chan peer.AddrInfo)
		close(peerOut)
		return peerOut
	}

	chSize := count
	if count == 0 {
		chSize = 1
	}
	peerOut := make(chan peer.AddrInfo, chSize)

	keyMH := key.Hash()

	logger.Debugw("finding providers", "cid", key, "mh", loggableProviderRecordBytes(keyMH))
	go dht.findProvidersAsyncRoutineForTest(ctx, key, keyMH, count, peerOut)
	return peerOut
}

// ipfs dht find prov
func (dht *IpfsDHT) findProvidersAsyncRoutineForTest(ctx context.Context, c cid.Cid, key multihash.Multihash, count int, peerOut chan peer.AddrInfo) {
	defer close(peerOut)
	// count = 20 false
	findAll := count == 0
	var ps *peer.Set
	if findAll {
		ps = peer.NewSet()
	} else {
		ps = peer.NewLimitedSet(count)
	}

	if dht.enableBF {
		var provs []peer.ID
		for p, bf := range dht.BFList {
			if bf.HasString(c.String()) {
				fmt.Printf("@lry: 本机BF列表找到提供节点 peer=%s!\n", p)
				provs = append(provs, p)
				break
			}
		}
		if len(provs) > 0 {
			for _, p := range provs {
				// NOTE: Assuming that this list of peers is unique
				if ps.TryAdd(p) {
					pi := dht.peerstore.PeerInfo(p)
					select {
					case peerOut <- pi:
					case <-ctx.Done():
						return
					}
				}

				// If we have enough peers locally, don't bother with remote RPC
				// TODO: is this a DOS vector?
				if !findAll && ps.Size() >= count {
					return
				}
			}
		} else {
			fmt.Printf("@lry: 本机BF列表未找到提供节点\n")
		}
	}

	// 迭代过程，改成找到一个提供者就停止的模式 并计算请求跳数 打印
	// 和findPeer迭代过程一样，一直迭代到除了unreachable之外所有节点中最接近key的beta个节点是queried的，所以扩散prov记录节点对迭代的终止没有什么影响，都是要迭代到最接近节点的
	lookupRes, err := dht.runLookupWithFollowup(ctx, string(key),
		func(ctx context.Context, p peer.ID) ([]*peer.AddrInfo, error) {
			// For DHT query command
			routing.PublishQueryEvent(ctx, &routing.QueryEvent{
				Type: routing.SendingQuery,
				ID:   p,
			})

			// 向一个节点请求
			fmt.Printf("@lry 向节点 %s 请求Prov \n", p)
			dht.jump++
			pmes, err := dht.findProvidersSingle(ctx, p, key)
			if err != nil {
				return nil, err
			}

			logger.Debugf("%d provider entries", len(pmes.GetProviderPeers()))
			provs := pb.PBPeersToPeerInfos(pmes.GetProviderPeers())
			logger.Debugf("%d provider entries decoded", len(provs))

			// Add unique providers from request, up to 'count'
			for _, prov := range provs {
				dht.maybeAddAddrs(prov.ID, prov.Addrs, peerstore.TempAddrTTL)
				logger.Debugf("got provider: %s", prov)
				if ps.TryAdd(prov.ID) {
					logger.Debugf("using provider: %s", prov)
					select {
					case peerOut <- *prov:
					case <-ctx.Done():
						logger.Debug("context timed out sending more providers")
						return nil, ctx.Err()
					}
				}
				// if !findAll && ps.Size() >= count {
				// 	logger.Debugf("got enough providers (%d/%d)", ps.Size(), count)
				// 	return nil, nil
				// }
				if !findAll && ps.Size() >= 1 {
					logger.Debugf("got enough providers (%d/%d)", ps.Size(), count)
					return nil, nil
				}
			}

			// Give closer peers back to the query to be queried
			closer := pmes.GetCloserPeers()
			peers := pb.PBPeersToPeerInfos(closer)
			logger.Debugf("got closer peers: %d %s", len(peers), peers)

			routing.PublishQueryEvent(ctx, &routing.QueryEvent{
				Type:      routing.PeerResponse,
				ID:        p,
				Responses: peers,
			})

			return peers, nil
		},
		func() bool {
			// findProv 终止条件，改成找到即停

			// return !findAll && ps.Size() >= count
			return !findAll && ps.Size() >= 1
		},
	)

	if err == nil && ctx.Err() == nil {
		dht.refreshRTIfNoShortcut(kb.ConvertKey(string(key)), lookupRes)
	}
}


// FindProvidersAsync is the same thing as FindProviders, but returns a channel.
// Peers will be returned on the channel as soon as they are found, even before
// the search query completes. If count is zero then the query will run until it
// completes. Note: not reading from the returned channel may block the query
// from progressing.
func (dht *IpfsDHT) FindProvidersAsync(ctx context.Context, key cid.Cid, count int) <-chan peer.AddrInfo {
	if !dht.enableProviders || !key.Defined() {
		peerOut := make(chan peer.AddrInfo)
		close(peerOut)
		return peerOut
	}

	chSize := count
	if count == 0 {
		chSize = 1
	}
	peerOut := make(chan peer.AddrInfo, chSize)

	keyMH := key.Hash()

	logger.Debugw("finding providers", "cid", key, "mh", loggableProviderRecordBytes(keyMH))
	go dht.findProvidersAsyncRoutine(ctx, keyMH, count, peerOut)
	return peerOut
}

func (dht *IpfsDHT) findProvidersAsyncRoutine(ctx context.Context, key multihash.Multihash, count int, peerOut chan peer.AddrInfo) {
	defer close(peerOut)

	findAll := count == 0
	var ps *peer.Set
	if findAll {
		ps = peer.NewSet()
	} else {
		ps = peer.NewLimitedSet(count)
	}

	provs := dht.ProviderManager.GetProviders(ctx, key)
	for _, p := range provs {
		// NOTE: Assuming that this list of peers is unique
		if ps.TryAdd(p) {
			pi := dht.peerstore.PeerInfo(p)
			select {
			case peerOut <- pi:
			case <-ctx.Done():
				return
			}
		}

		// If we have enough peers locally, don't bother with remote RPC
		// TODO: is this a DOS vector?
		if !findAll && ps.Size() >= count {
			return
		}
	}

	if ps.Size() >= 1 {
		fmt.Printf("@lry: 本机DHT查找到 提供节点为%s\n", ps.Peers()[0])
	}
	fmt.Printf("@lry: 请求key = %s \n", string(key))

	for p, bf := range dht.BFList {
		fmt.Printf("@lry: peer = %s BF search key = %s!\n", p, string(key))
		if bf.HasString(string(string(key))) {
			fmt.Printf("@lry: 本机BF列表找到提供节点 peer=%s!\n", p)
			provs = append(provs, p)
			return
		}
	}
	fmt.Printf("@lry: 本机BF列表未找到提供节点\n")

	lookupRes, err := dht.runLookupWithFollowup(ctx, string(key),
		func(ctx context.Context, p peer.ID) ([]*peer.AddrInfo, error) {
			// For DHT query command
			routing.PublishQueryEvent(ctx, &routing.QueryEvent{
				Type: routing.SendingQuery,
				ID:   p,
			})

			pmes, err := dht.findProvidersSingle(ctx, p, key)
			if err != nil {
				return nil, err
			}

			logger.Debugf("%d provider entries", len(pmes.GetProviderPeers()))
			provs := pb.PBPeersToPeerInfos(pmes.GetProviderPeers())
			logger.Debugf("%d provider entries decoded", len(provs))

			// Add unique providers from request, up to 'count'
			for _, prov := range provs {
				dht.maybeAddAddrs(prov.ID, prov.Addrs, peerstore.TempAddrTTL)
				logger.Debugf("got provider: %s", prov)
				if ps.TryAdd(prov.ID) {
					logger.Debugf("using provider: %s", prov)
					select {
					case peerOut <- *prov:
					case <-ctx.Done():
						logger.Debug("context timed out sending more providers")
						return nil, ctx.Err()
					}
				}
				if !findAll && ps.Size() >= 1 {
					logger.Debugf("got enough providers (%d/%d)", ps.Size(), count)
					return nil, nil
				}
			}

			// Give closer peers back to the query to be queried
			closer := pmes.GetCloserPeers()
			peers := pb.PBPeersToPeerInfos(closer)
			logger.Debugf("got closer peers: %d %s", len(peers), peers)

			routing.PublishQueryEvent(ctx, &routing.QueryEvent{
				Type:      routing.PeerResponse,
				ID:        p,
				Responses: peers,
			})

			return peers, nil
		},
		func() bool {
			// return !findAll && ps.Size() >= count
			fmt.Printf("@lry: 网络中找到提供者 stop() \n ")
			return !findAll && ps.Size() >= 1
		},
	)

	if err == nil && ctx.Err() == nil {
		dht.refreshRTIfNoShortcut(kb.ConvertKey(string(key)), lookupRes)
	}
}

// FindPeer searches for a peer with given ID.
func (dht *IpfsDHT) FindPeer(ctx context.Context, id peer.ID) (_ peer.AddrInfo, err error) {
	if err := id.Validate(); err != nil {
		return peer.AddrInfo{}, err
	}

	logger.Debugw("finding peer", "peer", id)

	// Check if were already connected to them
	if pi := dht.FindLocal(id); pi.ID != "" {
		return pi, nil
	}

	lookupRes, err := dht.runLookupWithFollowup(ctx, string(id),
		func(ctx context.Context, p peer.ID) ([]*peer.AddrInfo, error) {
			// For DHT query command
			routing.PublishQueryEvent(ctx, &routing.QueryEvent{
				Type: routing.SendingQuery,
				ID:   p,
			})

			pmes, err := dht.findPeerSingle(ctx, p, id)
			if err != nil {
				logger.Debugf("error getting closer peers: %s", err)
				return nil, err
			}
			peers := pb.PBPeersToPeerInfos(pmes.GetCloserPeers())

			// For DHT query command
			routing.PublishQueryEvent(ctx, &routing.QueryEvent{
				Type:      routing.PeerResponse,
				ID:        p,
				Responses: peers,
			})

			return peers, err
		},
		func() bool {
			return dht.host.Network().Connectedness(id) == network.Connected
		},
	)

	if err != nil {
		return peer.AddrInfo{}, err
	}

	dialedPeerDuringQuery := false
	for i, p := range lookupRes.peers {
		if p == id {
			// Note: we consider PeerUnreachable to be a valid state because the peer may not support the DHT protocol
			// and therefore the peer would fail the query. The fact that a peer that is returned can be a non-DHT
			// server peer and is not identified as such is a bug.
			dialedPeerDuringQuery = (lookupRes.state[i] == qpeerset.PeerQueried || lookupRes.state[i] == qpeerset.PeerUnreachable || lookupRes.state[i] == qpeerset.PeerWaiting)
			break
		}
	}

	// Return peer information if we tried to dial the peer during the query or we are (or recently were) connected
	// to the peer.
	connectedness := dht.host.Network().Connectedness(id)
	if dialedPeerDuringQuery || connectedness == network.Connected || connectedness == network.CanConnect {
		return dht.peerstore.PeerInfo(id), nil
	}

	return peer.AddrInfo{}, routing.ErrNotFound
}
