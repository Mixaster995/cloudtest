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
	"context"
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Mixaster995/cloudtest/pkg/utils"

	"github.com/Mixaster995/cloudtest/pkg/execmanager"

	"github.com/Mixaster995/cloudtest/pkg/model"

	"github.com/Mixaster995/cloudtest/pkg/config"
)

func TestUpdateTaskWithTimeout_ShouldNotCompleteTask(t *testing.T) {
	tmpDir, err := ioutil.TempDir(os.TempDir(), t.Name())
	require.NoError(t, err)
	defer utils.ClearFolder(tmpDir, false)
	ctx := executionContext{
		cloudTestConfig:  config.NewCloudTestConfig(),
		manager:          execmanager.NewExecutionManager(tmpDir),
		running:          make(map[string]*testTask),
		operationChannel: make(chan operationEvent, 1),
	}
	ctx.cloudTestConfig.Timeout = 2
	ctx.cloudTestConfig.Statistics.Enabled = false
	task := &testTask{
		test: &model.TestEntry{
			ExecutionConfig: &config.Execution{
				Timeout: 1,
			},
			Status: model.StatusSkipped,
		},
	}
	statsTimeout := time.Minute
	ctx.terminationChannel = make(chan error, len(ctx.cloudTestConfig.HealthCheck))
	RunHealthChecks(ctx.cloudTestConfig.HealthCheck, ctx.terminationChannel)
	termChannel := utils.NewOSSignalChannel()
	statTicker := time.NewTicker(statsTimeout)
	defer statTicker.Stop()

	ctx.tasks = append(ctx.tasks, task)
	ctx.updateTestExecution(task, "", model.StatusTimeout)
	_ = ctx.pollEvents(context.Background(), termChannel, statTicker.C)
	require.Len(t, ctx.completed, 0)
}
