/*
Copyright 2026 Kubernetes Authors.

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

package cache_test

import (
	"testing"

	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/cache"
	"k8s.io/apimachinery/pkg/labels"
)

func TestHashSelector_OrderIndependent(t *testing.T) {
	s1, err := labels.Parse("b=2,a=1")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := labels.Parse("a=1,b=2")
	if err != nil {
		t.Fatal(err)
	}

	if cache.HashSelector(s1) != cache.HashSelector(s2) {
		t.Error("HashSelector differs for the same requirements in different input order")
	}
}

func TestHashSelector_DiffersOnRealDifference(t *testing.T) {
	s1, err := labels.Parse("app=web")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := labels.Parse("app=api")
	if err != nil {
		t.Fatal(err)
	}

	if cache.HashSelector(s1) == cache.HashSelector(s2) {
		t.Error("HashSelector matched for genuinely different selectors")
	}
}

func TestHashSelector_NilTreatedAsEverything(t *testing.T) {
	if cache.HashSelector(nil) != cache.HashSelector(labels.Everything()) {
		t.Error("HashSelector(nil) != HashSelector(labels.Everything())")
	}
}

func TestKey_UsableAsMapKey(t *testing.T) {
	k1 := cache.Key{CatalogRevision: "r1", Verb: "get", MetricName: "cpu_avg_5m"}
	k2 := cache.Key{CatalogRevision: "r1", Verb: "get", MetricName: "cpu_avg_5m"}
	k3 := cache.Key{CatalogRevision: "r2", Verb: "get", MetricName: "cpu_avg_5m"}

	m := map[cache.Key]int{k1: 1}
	if _, ok := m[k2]; !ok {
		t.Error("equal Key values did not compare equal as map keys")
	}
	if _, ok := m[k3]; ok {
		t.Error("differing Key values compared equal as map keys")
	}
}
