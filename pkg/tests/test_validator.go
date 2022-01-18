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

package tests

import (
	"context"

	"github.com/Mixaster995/cloudtest/pkg/config"
	"github.com/Mixaster995/cloudtest/pkg/k8s"
)

type TestValidationFactory struct {
}

type testValidator struct {
	location string
	config   *config.ClusterProviderConfig
}

func (v *testValidator) WaitValid(context context.Context) error {
	return nil
}

func (v *testValidator) Validate() error {
	// Validation is passed for now
	return nil
}

func (*TestValidationFactory) CreateValidator(config *config.ClusterProviderConfig, location string) (k8s.KubernetesValidator, error) {
	return &testValidator{
		config:   config,
		location: location,
	}, nil
}
