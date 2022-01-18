// Copyright (c) 2019-2020 Cisco Systems, Inc and/or its affiliates.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package commands

import (
	"bufio"
	"context"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/Mixaster995/cloudtest/pkg/config"
	"github.com/Mixaster995/cloudtest/pkg/utils"
)

// RunHealthChecks - Start goroutines with health check probes
func RunHealthChecks(checkConfigs []*config.HealthCheckConfig, errCh chan<- error) {
	for i := range checkConfigs {
		go func(c int) {
			ready := true
			config := checkConfigs[c]
			for {
				interval := time.Duration(config.Interval) * time.Second
				<-time.After(interval)

				timeoutCtx, cancel := context.WithTimeout(context.Background(), interval)
				defer cancel()

				for _, cmd := range utils.ParseScript(config.Run) {
					builder := &strings.Builder{}
					_, err := utils.RunCommand(timeoutCtx, cmd, "", func(s string) {}, bufio.NewWriter(builder), nil, nil, false)
					if ready && err != nil {
						errCh <- errors.Wrapf(errors.Errorf(config.Message), "health check probe failed")
						return
					}
				}
			}
		}(i)
	}
}
