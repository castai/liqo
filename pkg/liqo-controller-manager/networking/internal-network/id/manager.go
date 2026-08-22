// Copyright 2019-2026 The Liqo Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package id

import (
	"fmt"
	"sync"
)

// Integer is the interface that represents an integer.
type Integer interface {
	int | int64 | int32 | int16 | int8 | uint | uint64 | uint32 | uint16 | uint8
}

// Manager is the struct that manages the Manager.
type Manager[T Integer] struct {
	allocatedMutex   sync.Mutex
	allocated        map[string]T // map[name]id
	allocatedReverse map[T]string // map[id]name
	minID            T
	nextAllocatedID  T
	maxID            T
}

// New returns a new Manager using IDs in the range [0, 2^24).
func New[T Integer]() *Manager[T] {
	// only 24 bits are used
	maxID := (1 << 24) - 1
	return NewWithRange[T](0, T(maxID))
}

// NewWithRange returns a new Manager using IDs in the range [minID, maxID).
func NewWithRange[T Integer](minID, maxID T) *Manager[T] {
	allocated := make(map[string]T)
	allocatedReverse := make(map[T]string)

	return &Manager[T]{
		allocated:        allocated,
		allocatedReverse: allocatedReverse,
		minID:            minID,
		nextAllocatedID:  minID,
		maxID:            maxID,
	}
}

// Configure configures the Manager with the given name and id.
// It is used to preallocate IDs already assigned in previous executions.
func (m *Manager[T]) Configure(name string, id T) error {
	m.allocatedMutex.Lock()
	defer m.allocatedMutex.Unlock()
	if _, ok := m.allocated[name]; ok {
		return fmt.Errorf("name %s already configured", name)
	}
	if _, ok := m.allocatedReverse[id]; ok {
		return fmt.Errorf("id %d already configured", id)
	}
	m.allocated[name] = id
	m.allocatedReverse[id] = name
	return nil
}

// Allocate allocates an ID for the given name.
func (m *Manager[T]) Allocate(name string) (T, error) {
	m.allocatedMutex.Lock()
	defer m.allocatedMutex.Unlock()
	if v, ok := m.allocated[name]; ok {
		return v, nil
	}

	var id T
	// First pass: search from the next candidate up to maxID.
	for id = m.nextAllocatedID; id < m.maxID; id++ {
		if _, ok := m.allocatedReverse[id]; !ok {
			if id == m.maxID-1 {
				m.nextAllocatedID = m.minID
			} else {
				m.nextAllocatedID = id + 1
			}
			m.allocated[name] = id
			m.allocatedReverse[id] = name
			return id, nil
		}
	}
	// Second pass: wrap around and search from minID up to nextAllocatedID.
	for id = m.minID; id < m.nextAllocatedID; id++ {
		if _, ok := m.allocatedReverse[id]; !ok {
			if id == m.maxID-1 {
				m.nextAllocatedID = m.minID
			} else {
				m.nextAllocatedID = id + 1
			}
			m.allocated[name] = id
			m.allocatedReverse[id] = name
			return id, nil
		}
	}
	return 0, fmt.Errorf("no more ID available")
}

// Release releases the ID allocated for the given name.
func (m *Manager[T]) Release(name string) {
	m.allocatedMutex.Lock()
	defer m.allocatedMutex.Unlock()
	if v, ok := m.allocated[name]; ok {
		delete(m.allocated, name)
		delete(m.allocatedReverse, v)
	}
}
