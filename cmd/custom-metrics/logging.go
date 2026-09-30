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
	"flag"
	"strings"

	"github.com/spf13/pflag"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
)

func installKlogFlags(fs *pflag.FlagSet) {
	klogFlags := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(klogFlags)
	fs.AddGoFlagSet(klogFlags)
}

// errStreamClosedText is the text of the HTTP/2 server's errStreamClosed,
// returned by a response write after the client has already reset its
// stream. The upstream response writers flatten it with %v before passing
// it to utilruntime.HandleError, so only the text survives.
const errStreamClosedText = "http2: stream closed"

// quietClientDisconnects wraps utilruntime.ErrorHandlers so a response
// write that failed only because the client closed its HTTP/2 stream first
// is logged at -v=4 instead of as an error. The aggregator's availability
// check does this routinely, and it is not a gateway failure. Every other
// error still reaches the original handlers (logging and rate limiting);
// their reported call site moves one frame, to utilruntime itself.
func quietClientDisconnects() {
	handlers := utilruntime.ErrorHandlers
	utilruntime.ErrorHandlers = []utilruntime.ErrorHandler{ //nolint:reassign // utilruntime's documented extension point
		func(ctx context.Context, err error, msg string, keysAndValues ...any) {
			if isClientDisconnect(err) {
				klog.FromContext(ctx).V(4).Info("Client closed the stream before the response was written", "err", err)

				return
			}
			for _, fn := range handlers {
				fn(ctx, err, msg, keysAndValues...)
			}
		},
	}
}

func isClientDisconnect(err error) bool {
	return err != nil && strings.Contains(err.Error(), errStreamClosedText)
}
