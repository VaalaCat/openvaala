package syncmap

// from https://gist.github.com/tarampampam/f96538257ff125ab71785710d48b3118

import "sync"

// SyncMap 保存可并发读写的键值；零值可直接使用，首次使用后不能复制。
type SyncMap[K comparable, V any] struct {
	mu sync.Mutex
	m  map[K]V
}

// Grow 在首次写入前按指定容量初始化映射。
func (s *SyncMap[K, V]) Grow(size int) {
	// 1. 持锁初始化映射，已初始化时保持不变。
	s.mu.Lock()
	s.grow(size)
	s.mu.Unlock()
}

// grow 初始化尚未创建的映射。
func (s *SyncMap[K, V]) grow(size ...int) {
	// 1. 映射为空时使用给定容量创建，未给容量时使用默认容量。
	if s.m == nil {
		if len(size) == 0 {
			s.m = make(map[K]V)
		} else {
			s.m = make(map[K]V, size[0])
		}
	}
}

// Clone 返回映射内容的副本。
func (s *SyncMap[K, V]) Clone() SyncMap[K, V] {
	// 1. 持锁复制全部键值，返回独立的 SyncMap。
	s.mu.Lock()
	defer s.mu.Unlock()

	var clone = make(map[K]V, len(s.m))

	for k, v := range s.m {
		clone[k] = v
	}

	return SyncMap[K, V]{m: clone}
}

// Load 读取键对应的值，同时返回是否找到。
func (s *SyncMap[K, V]) Load(key K) (value V, loaded bool) {
	// 1. 持锁查找键，映射未初始化时返回零值和 false。
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.m == nil {
		return
	}

	value, loaded = s.m[key]

	return
}

// Store 保存键对应的值。
func (s *SyncMap[K, V]) Store(key K, value V) {
	// 1. 持锁初始化映射并写入键值。
	s.mu.Lock()
	defer s.mu.Unlock()

	s.grow()

	s.m[key] = value
}

// LoadOrStore 键已存在时返回已有值，否则保存并返回传入值；loaded 说明返回的是不是已有值。
func (s *SyncMap[K, V]) LoadOrStore(key K, value V) (actual V, loaded bool) {
	// 1. 持锁查找键，不存在时初始化映射并写入传入值。
	s.mu.Lock()
	defer s.mu.Unlock()

	if actual, loaded = s.m[key]; !loaded {
		s.grow()

		s.m[key], actual = value, value
	}

	return
}

// LoadAndDelete 删除键并返回原值；loaded 说明是否找到。
func (s *SyncMap[K, V]) LoadAndDelete(key K) (value V, loaded bool) {
	// 1. 持锁读取并删除键；没找到时返回零值和 false。
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.m == nil {
		return
	}

	s.grow()

	if value, loaded = s.m[key]; loaded {
		delete(s.m, key)
	}

	return
}

// Delete 删除指定键。
func (s *SyncMap[K, V]) Delete(key K) {
	// 1. 持锁删除键，映射未初始化时直接返回。
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.m == nil {
		return
	}

	s.grow()

	delete(s.m, key)
}

// DeleteIf 键存在且 predicate 对它的值返回 true 时删除这个键，返回是否删除了；predicate 不能调用本 SyncMap 的方法。
func (s *SyncMap[K, V]) DeleteIf(key K, predicate func(V) bool) bool {
	// 1. 持锁读取值并执行 predicate，返回 true 时删除。
	s.mu.Lock()
	defer s.mu.Unlock()

	value, loaded := s.m[key]
	if !loaded || !predicate(value) {
		return false
	}
	delete(s.m, key)
	return true
}

// Range 逐个键值调用 f，f 返回 false 时停止；调用 f 时不持锁，f 里可以调用本 SyncMap 的方法。
func (s *SyncMap[K, V]) Range(f func(key K, value V) (shouldContinue bool)) {
	// 1. 映射未初始化时直接返回；否则持锁取下一个键值，调用 f 前释放锁，继续遍历前重新加锁；遍历期间内容可能变化，每个键最多访问一次。
	s.mu.Lock()

	if s.m == nil {
		s.mu.Unlock()

		return
	}

	s.grow()

	for k, v := range s.m {
		s.mu.Unlock()

		if !f(k, v) {
			return
		}

		s.mu.Lock()
	}

	s.mu.Unlock()
}

// Export 返回包含全部键值的普通 map 副本。
func (s *SyncMap[K, V]) Export() map[K]V {
	// 1. 持锁复制全部键值并返回。
	s.mu.Lock()
	defer s.mu.Unlock()

	m := make(map[K]V, len(s.m))

	for k, v := range s.m {
		m[k] = v
	}

	return m
}

// Len 返回已保存键值的数量。
func (s *SyncMap[K, V]) Len() (l int) {
	// 1. 持锁读取映射长度。
	s.mu.Lock()
	l = len(s.m)
	s.mu.Unlock()

	return
}

// Keys 返回全部键组成的切片。
func (s *SyncMap[K, V]) Keys() []K {
	// 1. 持锁收集全部键，顺序不固定。
	s.mu.Lock()
	defer s.mu.Unlock()

	var keys, i = make([]K, len(s.m)), 0

	for k := range s.m {
		keys[i], i = k, i+1
	}

	return keys
}

// Values 返回全部值组成的切片。
func (s *SyncMap[K, V]) Values() []V {
	// 1. 持锁收集全部值，顺序不固定。
	s.mu.Lock()
	defer s.mu.Unlock()

	var values, i = make([]V, len(s.m)), 0

	for _, v := range s.m {
		values[i], i = v, i+1
	}

	return values
}

// ToMap 返回包含全部键值的普通 map 副本。
func (s *SyncMap[K, V]) ToMap() map[K]V {
	// 1. 持锁复制全部键值并返回。
	s.mu.Lock()
	defer s.mu.Unlock()

	var m = make(map[K]V, len(s.m))

	for k, v := range s.m {
		m[k] = v
	}

	return m
}
