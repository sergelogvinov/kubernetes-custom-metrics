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

package cache

import (
	"crypto/sha256"
	"encoding/hex"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Key is the Tier-1 response-cache key (metric-gateway.md §4): every field
// that can affect a computed value, and nothing that identifies the
// caller — endpoint authorization already grants the corresponding data,
// and resolution always uses the same ServiceAccount regardless of which
// authorized caller asked. Every field is a plain
// comparable value, so Key can be used directly as a Go map key.
type Key struct {
	CatalogRevision    string
	Verb               string
	Namespace          string
	GroupResource      schema.GroupResource
	ObjectName         string
	MetricName         string
	ObjectSelectorHash string
	MetricSelectorHash string
}

// HashSelector canonicalizes selector into a short, deterministic string
// suitable for embedding in a Key. labels.Selector.String() already sorts
// requirements, so two selectors built from the same requirements in any
// input order hash identically; any differing requirement hashes
// differently. A nil selector is treated as labels.Everything().
func HashSelector(selector labels.Selector) string {
	if selector == nil {
		selector = labels.Everything()
	}

	sum := sha256.Sum256([]byte(selector.String()))

	return hex.EncodeToString(sum[:])
}
