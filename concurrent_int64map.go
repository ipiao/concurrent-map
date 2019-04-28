package cmap

import (
	"encoding/json"
	"sync"
)

// A "thread" safe map of type string:Anything.
// To avoid lock bottlenecks this map is dived to several (SHARD_COUNT) map shards.
type ConcurrentInt64Map []*ConcurrentInt64MapShared

// A "thread" safe int64 to anything map.
type ConcurrentInt64MapShared struct {
	items        map[int64]interface{}
	sync.RWMutex // Read Write mutex, guards access to internal map.
}

// Creates a new concurrent map.
func NewInt64() ConcurrentInt64Map {
	m := make(ConcurrentInt64Map, SHARD_COUNT)
	for i := 0; i < SHARD_COUNT; i++ {
		m[i] = &ConcurrentInt64MapShared{items: make(map[int64]interface{})}
	}
	return m
}

// GetShard returns shard under given key
func (m ConcurrentInt64Map) GetShard(key int64) *ConcurrentInt64MapShared {
	return m[key%int64(SHARD_COUNT)]
}

func (m ConcurrentInt64Map) MSet(data map[int64]interface{}) {
	for key, value := range data {
		shard := m.GetShard(key)
		shard.Lock()
		shard.items[key] = value
		shard.Unlock()
	}
}

// Sets the given value under the specified key.
func (m ConcurrentInt64Map) Set(key int64, value interface{}) {
	// Get map shard.
	shard := m.GetShard(key)
	shard.Lock()
	shard.items[key] = value
	shard.Unlock()
}

// Callback to return new element to be inserted into the map
// It is called while lock is held, therefore it MUST NOT
// try to access other keys in same map, as it can lead to deadlock since
// Go sync.RWLock is not reentrant
type Int64UpsertCb func(exist bool, valueInMap interface{}, newValue interface{}) interface{}

// Insert or Update - updates existing element or inserts a new one using UpsertCb
func (m ConcurrentInt64Map) Upsert(key int64, value interface{}, cb Int64UpsertCb) (res interface{}) {
	shard := m.GetShard(key)
	shard.Lock()
	v, ok := shard.items[key]
	res = cb(ok, v, value)
	shard.items[key] = res
	shard.Unlock()
	return res
}

// Sets the given value under the specified key if no value was associated with it.
func (m ConcurrentInt64Map) SetIfAbsent(key int64, value interface{}) bool {
	// Get map shard.
	shard := m.GetShard(key)
	shard.Lock()
	_, ok := shard.items[key]
	if !ok {
		shard.items[key] = value
	}
	shard.Unlock()
	return !ok
}

// Get retrieves an element from map under given key.
func (m ConcurrentInt64Map) Get(key int64) (interface{}, bool) {
	// Get shard
	shard := m.GetShard(key)
	shard.RLock()
	// Get item from shard.
	val, ok := shard.items[key]
	shard.RUnlock()
	return val, ok
}

// Count returns the number of elements within the map.
func (m ConcurrentInt64Map) Count() int {
	count := 0
	for i := 0; i < SHARD_COUNT; i++ {
		shard := m[i]
		shard.RLock()
		count += len(shard.items)
		shard.RUnlock()
	}
	return count
}

// Looks up an item under specified key
func (m ConcurrentInt64Map) Has(key int64) bool {
	// Get shard
	shard := m.GetShard(key)
	shard.RLock()
	// See if element is within shard.
	_, ok := shard.items[key]
	shard.RUnlock()
	return ok
}

// Remove removes an element from the map.
func (m ConcurrentInt64Map) Remove(key int64) {
	// Try to get shard.
	shard := m.GetShard(key)
	shard.Lock()
	delete(shard.items, key)
	shard.Unlock()
}

// RemoveCb is a callback executed in a map.RemoveCb() call, while Lock is held
// If returns true, the element will be removed from the map
type Int64RemoveCb func(key int64, v interface{}, exists bool) bool

// RemoveCb locks the shard containing the key, retrieves its current value and calls the callback with those params
// If callback returns true and element exists, it will remove it from the map
// Returns the value returned by the callback (even if element was not present in the map)
func (m ConcurrentInt64Map) RemoveCb(key int64, cb Int64RemoveCb) bool {
	// Try to get shard.
	shard := m.GetShard(key)
	shard.Lock()
	v, ok := shard.items[key]
	remove := cb(key, v, ok)
	if remove && ok {
		delete(shard.items, key)
	}
	shard.Unlock()
	return remove
}

// Pop removes an element from the map and returns it
func (m ConcurrentInt64Map) Pop(key int64) (v interface{}, exists bool) {
	// Try to get shard.
	shard := m.GetShard(key)
	shard.Lock()
	v, exists = shard.items[key]
	delete(shard.items, key)
	shard.Unlock()
	return v, exists
}

// IsEmpty checks if map is empty.
func (m ConcurrentInt64Map) IsEmpty() bool {
	return m.Count() == 0
}

// Used by the Iter & IterBuffered functions to wrap two variables together over a channel,
type Int64Tuple struct {
	Key int64
	Val interface{}
}

// Iter returns an iterator which could be used in a for range loop.
//
// Deprecated: using IterBuffered() will get a better performence
func (m ConcurrentInt64Map) Iter() <-chan Int64Tuple {
	chans := int64Snapshot(m)
	ch := make(chan Int64Tuple)
	go int64FanIn(chans, ch)
	return ch
}

// IterBuffered returns a buffered iterator which could be used in a for range loop.
func (m ConcurrentInt64Map) IterBuffered() <-chan Int64Tuple {
	chans := int64Snapshot(m)
	total := 0
	for _, c := range chans {
		total += cap(c)
	}
	ch := make(chan Int64Tuple, total)
	go int64FanIn(chans, ch)
	return ch
}

// Returns a array of channels that contains elements in each shard,
// which likely takes a snapshot of `m`.
// It returns once the size of each buffered channel is determined,
// before all the channels are populated using goroutines.
func int64Snapshot(m ConcurrentInt64Map) (chans []chan Int64Tuple) {
	chans = make([]chan Int64Tuple, SHARD_COUNT)
	wg := sync.WaitGroup{}
	wg.Add(SHARD_COUNT)
	// Foreach shard.
	for index, shard := range m {
		go func(index int, shard *ConcurrentInt64MapShared) {
			// Foreach key, value pair.
			shard.RLock()
			chans[index] = make(chan Int64Tuple, len(shard.items))
			wg.Done()
			for key, val := range shard.items {
				chans[index] <- Int64Tuple{key, val}
			}
			shard.RUnlock()
			close(chans[index])
		}(index, shard)
	}
	wg.Wait()
	return chans
}

// fanIn reads elements from channels `chans` into channel `out`
func int64FanIn(chans []chan Int64Tuple, out chan Int64Tuple) {
	wg := sync.WaitGroup{}
	wg.Add(len(chans))
	for _, ch := range chans {
		go func(ch chan Int64Tuple) {
			for t := range ch {
				out <- t
			}
			wg.Done()
		}(ch)
	}
	wg.Wait()
	close(out)
}

// Items returns all items as map[int64]interface{}
func (m ConcurrentInt64Map) Items() map[int64]interface{} {
	tmp := make(map[int64]interface{})

	// Insert items to temporary map.
	for item := range m.IterBuffered() {
		tmp[item.Key] = item.Val
	}

	return tmp
}

// Iterator callback,called for every key,value found in
// maps. RLock is held for all calls for a given shard
// therefore callback sess consistent view of a shard,
// but not across the shards
type Int64IterCb func(key int64, v interface{})

// Callback based iterator, cheapest way to read
// all elements in a map.
func (m ConcurrentInt64Map) IterCb(fn Int64IterCb) {
	for idx := range m {
		shard := (m)[idx]
		shard.RLock()
		for key, value := range shard.items {
			fn(key, value)
		}
		shard.RUnlock()
	}
}

// Keys returns all keys as []int64
func (m ConcurrentInt64Map) Keys() []int64 {
	count := m.Count()
	ch := make(chan int64, count)
	go func() {
		// Foreach shard.
		wg := sync.WaitGroup{}
		wg.Add(SHARD_COUNT)
		for _, shard := range m {
			go func(shard *ConcurrentInt64MapShared) {
				// Foreach key, value pair.
				shard.RLock()
				for key := range shard.items {
					ch <- key
				}
				shard.RUnlock()
				wg.Done()
			}(shard)
		}
		wg.Wait()
		close(ch)
	}()

	// Generate keys
	keys := make([]int64, 0, count)
	for k := range ch {
		keys = append(keys, k)
	}
	return keys
}

//Reviles ConcurrentMap "private" variables to json marshal.
func (m ConcurrentInt64Map) MarshalJSON() ([]byte, error) {
	// Create a temporary map, which will hold all item spread across shards.
	tmp := make(map[int64]interface{})

	// Insert items to temporary map.
	for item := range m.IterBuffered() {
		tmp[item.Key] = item.Val
	}
	return json.Marshal(tmp)
}

// Concurrent map uses Interface{} as its value, therefor JSON Unmarshal
// will probably won't know which to type to unmarshal into, in such case
// we'll end up with a value of type map[string]interface{}, In most cases this isn't
// out value type, this is why we've decided to remove this functionality.

// func (m *ConcurrentMap) UnmarshalJSON(b []byte) (err error) {
// 	// Reverse process of Marshal.

// 	tmp := make(map[string]interface{})

// 	// Unmarshal into a single map.
// 	if err := json.Unmarshal(b, &tmp); err != nil {
// 		return nil
// 	}

// 	// foreach key,value pair in temporary map insert into our concurrent map.
// 	for key, val := range tmp {
// 		m.Set(key, val)
// 	}
// 	return nil
// }
