/*
Copyright 2013 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Copyright 2021 Antrea Authors
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

// Package consistenthash provides an implementation of a ring hash.
package consistenthash

import (
	"hash/crc32"
	"sort"
	"strconv"
	"strings"
)

type Hash func(data []byte) uint32

type Map struct {
	hash     Hash
	replicas int
	keys     []int // Sorted
	keySet   map[string]struct{}
	hashMap  map[int]string
}

func New(replicas int, fn Hash) *Map {
	m := &Map{
		replicas: replicas,
		hash:     fn,
		hashMap:  make(map[int]string),
		keySet:   make(map[string]struct{}),
	}
	if m.hash == nil {
		m.hash = crc32.ChecksumIEEE
	}
	return m
}

// IsEmpty returns true if there are no items available.
func (m *Map) IsEmpty() bool {
	return len(m.keys) == 0
}

// Add adds some keys to the hash.
func (m *Map) Add(keys ...string) {
	for _, key := range keys {
		if _, exist := m.keySet[key]; exist {
			continue
		}
		m.keySet[key] = struct{}{}
		for i := 0; i < m.replicas; i++ {
			hash := int(m.hash([]byte(strconv.Itoa(i) + key)))
			m.keys = append(m.keys, hash)
			m.hashMap[hash] = key
		}
	}
	sort.Ints(m.keys)
}

// Remove removes keys from existing hash ring.
func (m *Map) Remove(keys ...string) {
	toRemove := make(map[int]struct{})
	for _, key := range keys {
		if _, exist := m.keySet[key]; !exist {
			continue
		}
		for i := 0; i < m.replicas; i++ {
			hash := int(m.hash([]byte(strconv.Itoa(i) + key)))
			toRemove[hash] = struct{}{}
		}
		delete(m.keySet, key)
	}
	var i, j int
	for j < len(m.keys) {
		if _, ok := toRemove[m.keys[j]]; !ok {
			m.keys[i] = m.keys[j]
			i++
		}
		j++
	}
	m.keys = m.keys[:i]
}

// Get gets the closest item in the hash to the provided key.
func (m *Map) Get(key string) string {
	return m.GetWithFilters(key)
}

// Get gets the closest item in the hash to the provided key with filters.
func (m *Map) GetWithFilters(key string, filters ...func(string) bool) string {
	if m.IsEmpty() {
		return ""
	}
	hash := int(m.hash([]byte(key)))
	// Binary search for appropriate replica.
	idx := sort.Search(len(m.keys), func(i int) bool { return m.keys[i] >= hash })
	// Check from idx and find the closest item which passes all filters. If no one is valid, return
	// an empty string.
	visited := make(map[string]struct{})
	for i := idx; i < idx+len(m.keys); i++ {
		// All keys visited. No valid key could pass all filters.
		if len(visited) == len(m.keySet) {
			break
		}
		actualIndex := i
		if actualIndex >= len(m.keys) {
			actualIndex -= len(m.keys)
		}
		v := m.hashMap[m.keys[actualIndex]]
		if _, ok := visited[v]; ok {
			continue
		}
		visited[v] = struct{}{}
		valid := true
		for _, f := range filters {
			if !f(v) {
				valid = false
				break
			}
		}
		if valid {
			return v
		}
	}
	return ""
}

func (m *Map) String() string {
	var keys []string
	for _, i := range m.keys {
		keys = append(keys, m.hashMap[i])
	}
	return strings.Join(keys, ",")
}
