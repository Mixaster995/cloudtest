// Copyright (c) 2020 Doc.ai and/or its affiliates.
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
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Mixaster995/cloudtest/pkg/config"
	"github.com/Mixaster995/cloudtest/pkg/execmanager"
	"github.com/Mixaster995/cloudtest/pkg/tests"
	"github.com/Mixaster995/cloudtest/pkg/utils"
)

func TestClusterInstanceStates(t *testing.T) {

	tmpDir, err := ioutil.TempDir(os.TempDir(), t.Name())
	require.NoError(t, err)
	defer utils.ClearFolder(tmpDir, false)

	testConfig := config.NewCloudTestConfig()
	testConfig.Timeout = 300
	testConfig.ConfigRoot = tmpDir
	testConfig.Providers = []*config.ClusterProviderConfig{
		createProvider(testConfig, "a_provider", "echo starting"),
		createProvider(testConfig, "b_provider", "echo starting\nexit 2"),
	}
	testConfig.Executions = append(testConfig.Executions, &config.Execution{
		Name:        "simple",
		Timeout:     15,
		PackageRoot: "./sample",
		TestsFound:  1,
	})

	ctx := executionContext{
		cloudTestConfig:  testConfig,
		manager:          execmanager.NewExecutionManager(tmpDir),
		running:          make(map[string]*testTask),
		operationChannel: make(chan operationEvent, 1),
		factory:          &tests.TestValidationFactory{},
		arguments: &Arguments{
			clusters: []string{
				"a_provider",
				"b_provider",
			},
		},
	}
	ctx.cloudTestConfig.Timeout = 2
	ctx.cloudTestConfig.Statistics.Enabled = false

	err = ctx.createClusters()
	require.NoError(t, err)

	require.Len(t, ctx.clusters, 2)
	require.Len(t, ctx.clusters[0].instances, 1)

	ctx.startCluster(ctx.clusters[0].instances[0])
	ctx.startCluster(ctx.clusters[1].instances[0])

	require.Eventually(t, func() bool {
		return ctx.clusters[0].instances[0].state.load() == clusterReady &&
			ctx.clusters[1].instances[0].state.load() == clusterCrashed
	}, 1*time.Second, 100*time.Millisecond, "Not equal: \n"+
		"expected: %v, %v\n"+
		"actual  : %v, %v",
		clusterReady, clusterCrashed,
		ctx.clusters[0].instances[0].state.load(), ctx.clusters[1].instances[0].state.load(),
	)

	ctx.clusters[0].instances[0].state.store(clusterStarting)

	err = ctx.destroyCluster(ctx.clusters[0].instances[0], false, false)
	require.NoError(t, err)
	require.Equal(t, ctx.clusters[0].instances[0].state.load(), clusterStarting)

	ctx.clusters[0].instances[0].state.store(clusterStopping)

	err = ctx.destroyCluster(ctx.clusters[0].instances[0], false, false)
	require.NoError(t, err)
	require.Equal(t, ctx.clusters[0].instances[0].state.load(), clusterCrashed)
}

func createProvider(testConfig *config.CloudTestConfig, name, startScript string) *config.ClusterProviderConfig {
	provider := &config.ClusterProviderConfig{
		Timeout:    100,
		Name:       name,
		NodeCount:  1,
		Kind:       "shell",
		RetryCount: 1,
		Instances:  1,
		Scripts: map[string]string{
			"config":  "echo ./.tests/config",
			"start":   startScript,
			"prepare": "echo prepared",
			"install": "echo installed",
			"stop":    "echo stopped",
		},
		Enabled: true,
	}
	testConfig.Providers = append(testConfig.Providers, provider)
	return provider
}
