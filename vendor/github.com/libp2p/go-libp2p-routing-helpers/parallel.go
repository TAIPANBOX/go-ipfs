package routinghelpers

import (
	"bytes"
	"context"
	"reflect"
	"sync"

	ci "github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/routing"

	multierror "github.com/hashicorp/go-multierror"
	cid "github.com/ipfs/go-cid"
	record "github.com/libp2p/go-libp2p-record"
)

// Parallel operates on the slice of routers in parallel.
type Parallel struct {
	Routers   []routing.Routing
	Validator record.Validator
}

// Helper function that sees through router composition to avoid unnecessary
// go routines.
func supportsKey(vs routing.ValueStore, key string) bool {
	switch vs := vs.(type) {
	case Null:
		return false
	case *Compose:
		return vs.ValueStore != nil && supportsKey(vs.ValueStore, key)
	case Parallel:
		for _, ri := range vs.Routers {
			if supportsKey(ri, key) {
				return true
			}
		}
		return false
	case Tiered:
		for _, ri := range vs.Routers {
			if supportsKey(ri, key) {
				return true
			}
		}
		return false
	case *LimitedValueStore:
		return vs.KeySupported(key) && supportsKey(vs.ValueStore, key)
	default:
		return true
	}
}

func supportsPeer(vs routing.PeerRouting) bool {
	switch vs := vs.(type) {
	case Null:
		return false
	case *Compose:
		return vs.PeerRouting != nil && supportsPeer(vs.PeerRouting)
	case Parallel:
		for _, ri := range vs.Routers {
			if supportsPeer(ri) {
				return true
			}
		}
		return false
	case Tiered:
		for _, ri := range vs.Routers {
			if supportsPeer(ri) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

func supportsContent(vs routing.ContentRouting) bool {
	switch vs := vs.(type) {
	case Null:
		return false
	case *Compose:
		return vs.ContentRouting != nil && supportsContent(vs.ContentRouting)
	case Parallel:
		for _, ri := range vs.Routers {
			if supportsContent(ri) {
				return true
			}
		}
		return false
	case Tiered:
		for _, ri := range vs.Routers {
			if supportsContent(ri) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

func (r Parallel) filter(filter func(routing.Routing) bool) Parallel {
	cpy := make([]routing.Routing, 0, len(r.Routers))
	for _, ri := range r.Routers {
		if filter(ri) {
			cpy = append(cpy, ri)
		}
	}
	return Parallel{Routers: cpy, Validator: r.Validator}
}

func (r Parallel) put(do func(routing.Routing) error) error {
	switch len(r.Routers) {
	case 0:
		return routing.ErrNotSupported
	case 1:
		return do(r.Routers[0])
	}

	var wg sync.WaitGroup
	results := make([]error, len(r.Routers))
	wg.Add(len(r.Routers))
	for i, ri := range r.Routers {
		go func(ri routing.Routing, i int) {
			results[i] = do(ri)
			wg.Done()
		}(ri, i)
	}
	wg.Wait()

	var (
		errs    []error
		success bool
	)
	for _, err := range results {
		switch err {
		case nil:
			// at least one router supports this.
			success = true
		case routing.ErrNotSupported:
		default:
			errs = append(errs, err)
		}
	}

	switch len(errs) {
	case 0:
		if success {
			// No errors and at least one router succeeded.
			return nil
		}
		// No routers supported this operation.
		return routing.ErrNotSupported
	case 1:
		return errs[0]
	default:
		return &multierror.Error{Errors: errs}
	}
}

func (r Parallel) search(ctx context.Context, do func(routing.Routing) (<-chan []byte, error)) (<-chan []byte, error) {
	switch len(r.Routers) {
	case 0:
		return nil, routing.ErrNotFound
	case 1:
		return do(r.Routers[0])
	}

	ctx, cancel := context.WithCancel(ctx)

	out := make(chan []byte)
	var errs []error

	var wg sync.WaitGroup
	for _, ri := range r.Routers {
		vchan, err := do(ri)
		switch err {
		case nil:
		case routing.ErrNotFound, routing.ErrNotSupported:
			continue
		default:
			errs = append(errs, err)
		}

		wg.Add(1)
		go func() {
			var sent int
			defer wg.Done()

			for {
				select {
				case v, ok := <-vchan:
					if !ok {
						if sent > 0 {
							cancel()
						}
						return
					}

					select {
					case out <- v:
						sent++
					case <-ctx.Done():
						return
					}
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	go func() {
		wg.Wait()
		close(out)
		cancel()
	}()

	return out, nil
}

func (r Parallel) get(ctx context.Context, do func(routing.Routing) (interface{}, error)) (interface{}, error) {
	switch len(r.Routers) {
	case 0:
		return nil, routing.ErrNotFound
	case 1:
		return do(r.Routers[0])
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan struct {
		val interface{}
		err error
	})
	for _, ri := range r.Routers {
		go func(ri routing.Routing) {
			value, err := do(ri)
			select {
			case results <- struct {
				val interface{}
				err error
			}{
				val: value,
				err: err,
			}:
			case <-ctx.Done():
			}
		}(ri)
	}

	var errs []error
	for range r.Routers {
		select {
		case res := <-results:
			switch res.err {
			case nil:
				return res.val, nil
			case routing.ErrNotFound, routing.ErrNotSupported:
				continue
			}
			// If the context has expired, just return that error
			// and ignore the other errors.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			errs = append(errs, res.err)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	switch len(errs) {
	case 0:
		return nil, routing.ErrNotFound
	case 1:
		return nil, errs[0]
	default:
		return nil, &multierror.Error{Errors: errs}
	}
}

func (r Parallel) forKey(key string) Parallel {
	return r.filter(func(ri routing.Routing) bool {
		return supportsKey(ri, key)
	})
}

func (r Parallel) PutValue(ctx context.Context, key string, value []byte, opts ...routing.Option) error {
	return r.forKey(key).put(func(ri routing.Routing) error {
		return ri.PutValue(ctx, key, value, opts...)
	})
}

func (r Parallel) GetValue(ctx context.Context, key string, opts ...routing.Option) ([]byte, error) {
	vInt, err := r.forKey(key).get(ctx, func(ri routing.Routing) (interface{}, error) {
		return ri.GetValue(ctx, key, opts...)
	})
	val, _ := vInt.([]byte)
	return val, err
}

func (r Parallel) SearchValue(ctx context.Context, key string, opts ...routing.Option) (<-chan []byte, error) {
	resCh, err := r.forKey(key).search(ctx, func(ri routing.Routing) (<-chan []byte, error) {
		return ri.SearchValue(ctx, key, opts...)
	})
	if err != nil {
		return nil, err
	}

	valid := make(chan []byte)
	var best []byte
	go func() {
		defer close(valid)

		for v := range resCh {
			if best != nil {
				n, err := r.Validator.Select(key, [][]byte{best, v})
				if err != nil {
					continue
				}
				if n != 1 {
					continue
				}
			}
			if bytes.Equal(best, v) && len(v) != 0 {
				continue
			}

			best = v
			select {
			case valid <- v:
			case <-ctx.Done():
				return
			}
		}
	}()

	return valid, err
}

func (r Parallel) GetPublicKey(ctx context.Context, p peer.ID) (ci.PubKey, error) {
	vInt, err := r.
		forKey(routing.KeyForPublicKey(p)).
		get(ctx, func(ri routing.Routing) (interface{}, error) {
			return routing.GetPublicKey(ri, ctx, p)
		})
	val, _ := vInt.(ci.PubKey)
	return val, err
}

func (r Parallel) FindPeer(ctx context.Context, p peer.ID) (peer.AddrInfo, error) {
	vInt, err := r.filter(func(ri routing.Routing) bool {
		return supportsPeer(ri)
	}).get(ctx, func(ri routing.Routing) (interface{}, error) {
		return ri.FindPeer(ctx, p)
	})
	pi, _ := vInt.(peer.AddrInfo)
	return pi, err
}

func (r Parallel) Provide(ctx context.Context, c cid.Cid, local bool) error {
	return r.filter(func(ri routing.Routing) bool {
		return supportsContent(ri)
	}).put(func(ri routing.Routing) error {
		return ri.Provide(ctx, c, local)
	})
}

func (r Parallel) FindProvidersAsync(ctx context.Context, c cid.Cid, count int) <-chan peer.AddrInfo {
	routers := r.filter(func(ri routing.Routing) bool {
		return supportsContent(ri)
	})

	switch len(routers.Routers) {
	case 0:
		ch := make(chan peer.AddrInfo)
		close(ch)
		return ch
	case 1:
		return routers.Routers[0].FindProvidersAsync(ctx, c, count)
	}

	out := make(chan peer.AddrInfo)

	ctx, cancel := context.WithCancel(ctx)

	providers := make([]<-chan peer.AddrInfo, len(routers.Routers))
	for i, ri := range routers.Routers {
		providers[i] = ri.FindProvidersAsync(ctx, c, count)
	}

	go func() {
		defer cancel()
		defer close(out)
		if len(providers) > 8 {
			manyProviders(ctx, out, providers, count)
		} else {
			fewProviders(ctx, out, providers, count)
		}
	}()
	return out
}

// Unoptimized many provider case. Doing this with reflection is a bit slow but
// definitely simpler. If we start having more than 8 peer routers running in
// parallel, we can revisit this.
func manyProviders(ctx context.Context, out chan<- peer.AddrInfo, in []<-chan peer.AddrInfo, count int) {
	found := make(map[peer.ID]struct{}, count)

	selectCases := make([]reflect.SelectCase, len(in))
	for i, ch := range in {
		selectCases[i] = reflect.SelectCase{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(ch),
		}
	}
	for count > 0 && len(selectCases) > 0 {
		chosen, val, ok := reflect.Select(selectCases)
		if !ok {
			// Remove the channel
			selectCases[chosen] = selectCases[len(selectCases)-1]
			selectCases = selectCases[:len(selectCases)-1]
			continue
		}

		pi := val.Interface().(peer.AddrInfo)
		if _, ok := found[pi.ID]; ok {
			continue
		}

		select {
		case out <- pi:
			found[pi.ID] = struct{}{}
			count--
		case <-ctx.Done():
			return
		}
	}
}

// Optimization for few providers (<=8).
func fewProviders(ctx context.Context, out chan<- peer.AddrInfo, in []<-chan peer.AddrInfo, count int) {
	if len(in) > 8 {
		panic("case only valid for combining fewer than 8 channels")
	}

	found := make(map[peer.ID]struct{}, count)

	cases := make([]<-chan peer.AddrInfo, 8)
	copy(cases, in)

	// Oh go, what would we do without you!
	nch := len(in)
	var pi peer.AddrInfo
	for nch > 0 && count > 0 {
		var ok bool
		var selected int
		select {
		case pi, ok = <-cases[0]:
			selected = 0
		case pi, ok = <-cases[1]:
			selected = 1
		case pi, ok = <-cases[2]:
			selected = 2
		case pi, ok = <-cases[3]:
			selected = 3
		case pi, ok = <-cases[4]:
			selected = 4
		case pi, ok = <-cases[5]:
			selected = 5
		case pi, ok = <-cases[6]:
			selected = 6
		case pi, ok = <-cases[7]:
			selected = 7
		}
		if !ok {
			cases[selected] = nil
			nch--
			continue
		}
		if _, ok = found[pi.ID]; ok {
			continue
		}

		select {
		case out <- pi:
			found[pi.ID] = struct{}{}
			count--
		case <-ctx.Done():
			return
		}
	}
}

func (r Parallel) Bootstrap(ctx context.Context) error {
	var me multierror.Error
	for _, b := range r.Routers {
		if err := b.Bootstrap(ctx); err != nil {
			me.Errors = append(me.Errors, err)
		}
	}
	return me.ErrorOrNil()
}

var _ routing.Routing = Parallel{}
