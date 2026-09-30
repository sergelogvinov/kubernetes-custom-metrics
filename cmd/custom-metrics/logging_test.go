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

package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
)

func TestQuietClientDisconnects(t *testing.T) {
	saved := utilruntime.ErrorHandlers
	t.Cleanup(func() { utilruntime.ErrorHandlers = saved }) //nolint:reassign

	var got []error
	utilruntime.ErrorHandlers = []utilruntime.ErrorHandler{ //nolint:reassign
		func(_ context.Context, err error, _ string, _ ...any) { got = append(got, err) },
	}
	quietClientDisconnects()

	// The upstream response writer flattens the error with %v.
	utilruntime.HandleError(fmt.Errorf("apiserver was unable to write a JSON response: %v", errors.New("http2: stream closed")))
	other := errors.New("apiserver was unable to write a JSON response: boom")
	utilruntime.HandleError(other)

	if len(got) != 1 || got[0] != other {
		t.Errorf("original handlers saw %v, want only %v", got, other)
	}
}
