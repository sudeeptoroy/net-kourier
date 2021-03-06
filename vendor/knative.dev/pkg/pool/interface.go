/*
Copyright 2020 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package pool

import (
	"golang.org/x/sync/errgroup"
)

// Interface defines an errgroup-compatible interface for interacting with
// our threadpool.
type Interface interface {
	// Go queues a single unit of work for execution on this pool. All calls
	// to Go must be finished before Wait is called.
	Go(func() error)

	// Wait blocks until all work is complete, returning the first
	// error returned by any of the work.
	Wait() error
}

// errgroup.Group implements Interface
var _ Interface = (*errgroup.Group)(nil)
