/*
   Copyright 2020 Docker Compose CLI authors

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

package mobycli

import (
	"testing"

	"gotest.tools/v3/assert"

	"github.com/docker/compose-cli/context/store"
)

func TestDelegateContextTypeToMoby(t *testing.T) {

	isDelegated := func(val string) bool {
		for _, ctx := range delegatedContextTypes {
			if ctx == val {
				return true
			}
		}
		return false
	}

	allCtx := []string{store.AciContextType, store.EcsContextType, store.AwsContextType, store.DefaultContextType}
	for _, ctx := range allCtx {
		if isDelegated(ctx) {
			assert.Assert(t, mustDelegateToMoby(ctx))
			continue
		}
		assert.Assert(t, !mustDelegateToMoby(ctx))
	}
}
