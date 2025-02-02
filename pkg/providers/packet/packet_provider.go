// Copyright (c) 2021 Doc.ai and/or its affiliates.
//
// Copyright (c) 2019-2021 Cisco Systems, Inc and/or its affiliates.
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

// Package packet provides utils for configuring packet cluster
package packet

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/packethost/packngo"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/networkservicemesh/cloudtest/pkg/config"
	"github.com/networkservicemesh/cloudtest/pkg/execmanager"
	"github.com/networkservicemesh/cloudtest/pkg/k8s"
	"github.com/networkservicemesh/cloudtest/pkg/providers"
	"github.com/networkservicemesh/cloudtest/pkg/shell"
	"github.com/networkservicemesh/cloudtest/pkg/utils"
)

const (
	installScript     = "install" // #1
	setupScript       = "setup"   // #2
	startScript       = "start"   // #3
	configScript      = "config"  // #4
	prepareScript     = "prepare" // #5
	stopScript        = "stop"    // #6
	cleanupScript     = "cleanup" // #7
	packetProjectID   = "PACKET_PROJECT_ID"
	queuedState       = "queued"
	provisioningState = "provisioning"
	activeState       = "active"
	failedState       = "failed"
)

type packetProvider struct {
	root    string
	indexes map[string]int
	sync.Mutex
	clusters    []packetInstance
	installDone map[string]bool
}

type packetInstance struct {
	installScript            []string
	setupScript              []string
	startScript              []string
	prepareScript            []string
	stopScript               []string
	manager                  execmanager.ExecutionManager
	root                     string
	id                       string
	configScript             string
	factory                  k8s.ValidationFactory
	validator                k8s.KubernetesValidator
	configLocation           string
	shellInterface           shell.Manager
	projectID                string
	packetAuthKey            string
	keyID                    string
	config                   *config.ClusterProviderConfig
	provider                 *packetProvider
	client                   *packngo.Client
	project                  *packngo.Project
	devices                  map[string]string
	sshKey                   *packngo.SSHKey
	params                   providers.InstanceOptions
	started                  bool
	keyIds                   []string
	virtualNetworkList       []packngo.VirtualNetwork
	hardwareReservationsList []*packngo.HardwareReservation
	facilitiesList           []string
}

func (pi *packetInstance) GetID() string {
	return pi.id
}

func (pi *packetInstance) CheckIsAlive() error {
	if pi.started {
		return pi.validator.Validate()
	}
	return errors.New("cluster is not running")
}

func (pi *packetInstance) IsRunning() bool {
	return pi.started
}

func (pi *packetInstance) GetClusterConfig() (string, error) {
	if pi.started {
		return pi.configLocation, nil
	}
	return "", errors.New("cluster is not started yet")
}

func (pi *packetInstance) Start(timeout time.Duration) (string, error) {
	logrus.Infof("Starting cluster %s-%s", pi.config.Name, pi.id)
	var err error
	fileName := ""
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Set seed
	rand.Seed(time.Now().UnixNano())

	utils.ClearFolder(pi.root, true)

	// Process and prepare environment variables
	if err = pi.shellInterface.ProcessEnvironment(
		pi.id, pi.config.Name, pi.root, pi.config.Env, nil); err != nil {
		logrus.Errorf("error during processing environment variables %v", err)
		return "", err
	}

	// Do prepare
	if !pi.params.NoInstall {
		if fileName, err = pi.doInstall(ctx); err != nil {
			return fileName, err
		}
	}

	// Run start script
	if fileName, err = pi.shellInterface.RunCmd(ctx, "setup", pi.setupScript, nil); err != nil {
		return fileName, err
	}

	keyFile := pi.config.Packet.SSHKey
	if !utils.FileExists(keyFile) {
		// Relative file
		keyFile = path.Join(pi.root, keyFile)
		if !utils.FileExists(keyFile) {
			err = errors.New("failed to locate generated key file, please specify init script to generate it")
			logrus.Errorf(err.Error())
			return "", err
		}
	}

	if pi.client, err = packngo.NewClient(); err != nil {
		logrus.Errorf("failed to create Packet REST interface")
		return "", err
	}

	if err = pi.updateProject(); err != nil {
		return "", err
	}

	// Check and add key if it is not yet added.

	if pi.keyIds, err = pi.createKey(keyFile); err != nil {
		return "", err
	}

	var virtualNetworks *packngo.VirtualNetworkListResponse
	virtualNetworks, _, err = pi.client.ProjectVirtualNetworks.List(pi.projectID, nil)
	if err != nil {
		return "", err
	}
	pi.virtualNetworkList = virtualNetworks.VirtualNetworks

	portsVLANs := make(map[string]map[string]int)

	if pi.hardwareReservationsList, err = pi.findHardwareReservations(); err != nil {
		return "", err
	}
	for _, devCfg := range pi.config.Packet.HardwareDevices {
		devID, err := pi.createHardwareDevice(devCfg)
		if err != nil {
			return "", err
		}
		pi.devices[devCfg.Name] = devID

		if devCfg.PortVLANs != nil {
			portsVLANs[devCfg.Name] = devCfg.PortVLANs
		}
	}

	if pi.facilitiesList, err = pi.findFacilities(); err != nil {
		return "", err
	}
	for _, devCfg := range pi.config.Packet.Devices {
		devID, err := pi.createFacilityDevice(devCfg)
		if err != nil {
			return "", err
		}
		pi.devices[devCfg.Name] = devID

		if len(devCfg.PortVLANs) != 0 {
			portsVLANs[devCfg.Name] = devCfg.PortVLANs
		}
	}

	// All devices are created so we need to wait for them to get alive.
	if err = pi.waitDevicesStartup(ctx); err != nil {
		return "", err
	}

	// Setup ports
	for key, portVLANs := range portsVLANs {
		err = pi.setupDevicePorts(key, portVLANs)
		if err != nil {
			return "", err
		}
	}

	// We need to add arguments
	if err = pi.addDeviceContextArguments(); err != nil {
		return "", err
	}

	pi.manager.AddLog(pi.id, "environment", pi.shellInterface.PrintEnv(pi.shellInterface.GetProcessedEnv()))
	pi.manager.AddLog(pi.id, "arguments", pi.shellInterface.PrintArgs())

	// Run start script
	if fileName, err = pi.shellInterface.RunCmd(ctx, "start", pi.startScript, nil); err != nil {
		return fileName, err
	}

	if err = pi.updateKUBEConfig(ctx); err != nil {
		return "", err
	}

	if pi.validator, err = pi.factory.CreateValidator(pi.config, pi.configLocation); err != nil {
		msg := fmt.Sprintf("Failed to start validator %v", err)
		logrus.Errorf(msg)
		return "", err
	}
	// Run prepare script
	if fileName, err = pi.shellInterface.RunCmd(ctx, "prepare", pi.prepareScript, []string{"KUBECONFIG=" + pi.configLocation}); err != nil {
		return fileName, err
	}

	// Wait a bit to be sure clusters are up and running.
	st := time.Now()
	err = pi.validator.WaitValid(ctx)
	if err != nil {
		logrus.Errorf("Failed to wait for required number of nodes: %v", err)
		return fileName, err
	}
	logrus.Infof("Waiting for desired number of nodes complete %s-%s %v", pi.config.Name, pi.id, time.Since(st))

	pi.started = true
	logrus.Infof("Starting are up and running %s-%s", pi.config.Name, pi.id)
	return "", nil
}

func (pi *packetInstance) updateKUBEConfig(context context.Context) error {
	if pi.configLocation == "" {
		pi.configLocation = pi.shellInterface.GetConfigLocation()
	}
	if pi.configLocation == "" {
		output, err := utils.ExecRead(context, "", strings.Split(pi.configScript, " "))
		if err != nil {
			err = errors.Wrap(err, "failed to retrieve configuration location")
			logrus.Errorf(err.Error())
		}
		pi.configLocation = output[0]
	}
	return nil
}

func (pi *packetInstance) addDeviceContextArguments() error {
	for key, devID := range pi.devices {
		dev, _, err := pi.client.Devices.Get(devID, getOptions("ip_addresses"))
		if err != nil {
			return errors.Wrapf(err, "failed to fetch device networks info: %v", key)
		}
		for _, n := range dev.Network {
			pub := "pub"
			if !n.Public {
				pub = "private"
			}
			pi.shellInterface.AddExtraArgs(fmt.Sprintf("device.%v.%v.%v.%v", key, pub, "ip", n.AddressFamily), n.Address)
			pi.shellInterface.AddExtraArgs(fmt.Sprintf("device.%v.%v.%v.%v", key, pub, "gw", n.AddressFamily), n.Gateway)
			pi.shellInterface.AddExtraArgs(fmt.Sprintf("device.%v.%v.%v.%v", key, pub, "net", n.AddressFamily), n.Network)
		}
	}
	return nil
}

func (pi *packetInstance) waitDevicesStartup(context context.Context) error {
	_, file, err := pi.manager.OpenFile(pi.id, "wait-nodes")
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	log := utils.NewLogger(file)

	active := map[string]*packngo.Device{}
	failed := map[string]*packngo.Device{}
	for len(active)+len(failed) < len(pi.devices) {
		for key, devID := range pi.devices {
			var device *packngo.Device
			device, _, err := pi.client.Devices.Get(devID, nil)
			if err != nil {
				logrus.Errorf("%v-%v Error accessing device Error: %v", pi.id, devID, err)
				continue
			}
			log.Printf("Checking status %v %v %v\n", key, devID, device.State)
			logrus.Infof("%v-Checking status %v", pi.id, device.State)
			switch device.State {
			case activeState:
				active[key] = device
			case failedState:
				failed[key] = device
			}
		}
		select {
		case <-time.After(10 * time.Second):
			continue
		case <-context.Done():
			log.Println("Timeout")
			return errors.Wrap(context.Err(), "timeout")
		}
	}

	if len(failed) > 0 {
		log.Println("There are failed devices")
		return errors.Errorf("failed devices: %v", failed)
	}

	log.Println("All devices online")

	return nil
}

func (pi *packetInstance) createHardwareDevice(devCfg *config.HardwareDeviceConfig) (string, error) {
	devReq, err := pi.createRequest(devCfg)
	if err != nil {
		return "", err
	}

	for _, hr := range pi.hardwareReservationsList {
		devReq.Plan = hr.Plan.Slug
		devReq.Facility = []string{hr.Facility.Code}
		devReq.HardwareReservationID = hr.ID

		device, _, err := pi.client.Devices.Create(devReq)

		msg := fmt.Sprintf("HostName=%v\n%v", devReq.Hostname, err)
		logrus.Infof(fmt.Sprintf("%s-%v", pi.id, msg))
		pi.manager.AddLog(pi.id, fmt.Sprintf("create-device-%s", devCfg.Name), msg)

		switch {
		case err == nil:
			return device.ID, nil
		case strings.Contains(err.Error(), "is not provisionable"):
		case strings.Contains(err.Error(), "Oh snap, something went wrong"):
		default:
			return "", err
		}
	}

	return "", errors.New("empty hardware reservations list")
}

func (pi *packetInstance) createFacilityDevice(devCfg *config.FacilityDeviceConfig) (string, error) {
	devReq, err := pi.createRequest(&devCfg.HardwareDeviceConfig)
	if err != nil {
		return "", err
	}
	devReq.Plan = devCfg.Plan

	for i := range pi.facilitiesList {
		devReq.Facility = []string{pi.facilitiesList[i]}

		device, _, err := pi.client.Devices.Create(devReq)

		msg := fmt.Sprintf("HostName=%v\n%v", devReq.Hostname, err)
		logrus.Infof(fmt.Sprintf("%s-%v", pi.id, msg))
		pi.manager.AddLog(pi.id, fmt.Sprintf("create-device-%s", devCfg.Name), msg)

		switch {
		case err == nil:
			return device.ID, nil
		case strings.Contains(err.Error(), "has no provisionable"):
		case strings.Contains(err.Error(), "Oh snap, something went wrong"):
		default:
			return "", err
		}
	}

	return "", errors.New("empty facilities list")
}

func (pi *packetInstance) createRequest(devCfg *config.HardwareDeviceConfig) (*packngo.DeviceCreateRequest, error) {
	finalEnv := pi.shellInterface.GetProcessedEnv()

	environment := map[string]string{}
	for _, k := range finalEnv {
		key, value, err := utils.ParseVariable(k)
		if err != nil {
			return nil, err
		}
		environment[key] = value
	}
	var hostName string
	var err error
	if hostName, err = utils.SubstituteVariable(devCfg.HostName, environment, pi.shellInterface.GetArguments()); err != nil {
		return nil, err
	}

	return &packngo.DeviceCreateRequest{
		Hostname:       hostName,
		OS:             devCfg.OperatingSystem,
		BillingCycle:   devCfg.BillingCycle,
		ProjectID:      pi.projectID,
		ProjectSSHKeys: pi.keyIds,
	}, err
}

func (pi *packetInstance) setupDevicePorts(key string, portVLANs map[string]int) error {
	_, file, err := pi.manager.OpenFile(pi.id, "setup-device-network")
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	log := utils.NewLogger(file)

	defer func() {
		if err != nil {
			log.Printf("error: %v\n", err)
		}
	}()

	var device *packngo.Device
	for portName, vlanTag := range portVLANs {
		log.Printf("port to vlan: %v -> %v\n", portName, vlanTag)

		if device, _, err = pi.client.Devices.Get(pi.devices[key], getOptions()); err != nil {
			return err
		}

		var port *packngo.Port
		if port, err = device.GetPortByName(portName); err != nil {
			return err
		}

		if port.Data.Bonded {
			if _, _, err = pi.client.Ports.Disbond(port.ID, true); err != nil {
				return err
			}
		}

		var vlan *packngo.VirtualNetwork
		if vlan, err = pi.findVlan(vlanTag); err != nil {
			return err
		}

		if _, _, err = pi.client.Ports.Assign(port.ID, vlan.ID); err != nil {
			return err
		}
	}
	return nil
}

func (pi *packetInstance) findVlan(vlanTag int) (*packngo.VirtualNetwork, error) {
	for i := range pi.virtualNetworkList {
		if vlan := &pi.virtualNetworkList[i]; vlan.VXLAN == vlanTag {
			return vlan, nil
		}
	}
	return nil, errors.Errorf("vlan not found: %v", vlanTag)
}

func (pi *packetInstance) findHardwareReservations() ([]*packngo.HardwareReservation, error) {
	hardwareReservations, response, err := pi.client.HardwareReservations.List(pi.projectID, listOptions("facility"))

	if err != nil {
		pi.manager.AddLog(pi.id, "list-hardware-reservations", fmt.Sprintf("%v\n%v\n", response.String(), err))
		return nil, err
	}

	var hardwareReservationsList []*packngo.HardwareReservation
	for i := range hardwareReservations {
		hr := &hardwareReservations[i]
		for _, hrr := range pi.config.Packet.HardwareReservations {
			if hrr == hr.ID {
				hardwareReservationsList = append(hardwareReservationsList, hr)
			}
		}
	}
	return hardwareReservationsList, nil
}

func (pi *packetInstance) findFacilities() ([]string, error) {
	_, file, err := pi.manager.OpenFile(pi.id, "list-facilities")
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	log := utils.NewLogger(file)

	facilities, response, err := pi.client.Facilities.List(&packngo.ListOptions{})
	if err != nil {
		log.Printf("%v\n%v\n", response.String(), err)
		return nil, err
	}

	var facilitiesList []string
	for _, f := range facilities {
		facilityReqs := map[string]string{}
		for _, ff := range f.Features {
			facilityReqs[ff] = ff
		}

		found := true
		for _, ff := range pi.config.Packet.Facilities {
			if _, ok := facilityReqs[ff]; !ok {
				found = false
				break
			}
		}
		if found {
			facilitiesList = append(facilitiesList, f.Code)
		}
	}

	// Randomize facilities.
	ind := -1
	if pi.config.Packet.PreferredFacility != "" {
		for i, f := range facilitiesList {
			if f == pi.config.Packet.PreferredFacility {
				ind = i
				break
			}
		}
	}
	if ind != -1 {
		facilitiesList[ind], facilitiesList[0] = facilitiesList[0], facilitiesList[ind]
	}

	log.Printf("List of facilities: %v %v\n", facilities, response)

	return facilitiesList, nil
}

func (pi *packetInstance) Destroy(_ time.Duration) error {
	logrus.Infof("Destroying cluster  %s", pi.id)
	return nil
}

func (pi *packetInstance) GetRoot() string {
	return pi.root
}

func (pi *packetInstance) doInstall(context context.Context) (string, error) {
	pi.provider.Lock()
	defer pi.provider.Unlock()
	if pi.installScript != nil && !pi.provider.installDone[pi.config.Name] {
		pi.provider.installDone[pi.config.Name] = true
		return pi.shellInterface.RunCmd(context, "install", pi.installScript, nil)
	}
	return "", nil
}

func (pi *packetInstance) updateProject() error {
	ps, response, err := pi.client.Projects.List(nil)

	out := strings.Builder{}
	_, _ = out.WriteString(fmt.Sprintf("%v\n%v\n", response, err))

	if err != nil {
		logrus.Errorf("Failed to list Packet projects")
	}

	for i := 0; i < len(ps); i++ {
		p := &ps[i]
		_, _ = out.WriteString(fmt.Sprintf("Project: %v\n %v", p.Name, p))
		if p.ID == pi.projectID {
			pp := ps[i]
			pi.project = &pp
		}
	}

	pi.manager.AddLog(pi.id, "list-projects", out.String())

	if pi.project == nil {
		err := errors.Errorf("%s - specified project are not found on Packet %v", pi.id, pi.projectID)
		logrus.Errorf(err.Error())
		return err
	}
	return nil
}

func (pi *packetInstance) createKey(keyFile string) ([]string, error) {
	_, file, err := pi.manager.OpenFile(pi.id, "create-key")
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	log := utils.NewLogger(file)

	today := time.Now()
	genID := fmt.Sprintf("%d-%d-%d-%s", today.Year(), today.Month(), today.Day(), utils.NewRandomStr(10))
	pi.keyID = "dev-ci-cloud-" + genID

	keyFileContent, err := utils.ReadFile(keyFile)
	if err != nil {
		log.Printf("Failed to read key file %s\n", keyFile)
		logrus.Errorf("Failed to read file %v %v", keyFile, err)
		return nil, err
	}

	log.Printf("Key file %s readed ok\n", keyFile)

	keyRequest := &packngo.SSHKeyCreateRequest{
		ProjectID: pi.project.ID,
		Label:     pi.keyID,
		Key:       strings.Join(keyFileContent, "\n"),
	}
	sshKey, response, err := pi.client.SSHKeys.Create(keyRequest)

	responseMsg := ""
	if response != nil {
		responseMsg = response.String()
	}
	log.Printf("Create key %v %v %v\n", sshKey, responseMsg, err)

	keyIds := []string{}
	if sshKey == nil {
		// try to find key.
		sshKey, keyIds = pi.findKeys(log)
	} else {
		logrus.Infof("%s-Create key %v (%v)", pi.id, sshKey.ID, sshKey.Key)
		keyIds = append(keyIds, sshKey.ID)
	}
	pi.sshKey = sshKey
	log.Printf("%v\n%v\n%v\n %s\n", sshKey, response, err)

	if sshKey == nil {
		log.Printf("Failed to create ssh key %v %v", sshKey, err)
		logrus.Errorf("Failed to create ssh key %v", err)
		return nil, err
	}
	return keyIds, nil
}

func (pi *packetInstance) findKeys(log logrus.StdLogger) (sshKey *packngo.SSHKey, keyIDs []string) {
	sshKeys, response, err := pi.client.SSHKeys.List()
	if err != nil {
		log.Printf("List keys error %v %v\n", response, err)
	}
	for k := 0; k < len(sshKeys); k++ {
		kk := &sshKeys[k]
		if kk.Label == pi.keyID {
			sshKey = &packngo.SSHKey{
				ID:          kk.ID,
				Label:       kk.Label,
				URL:         kk.URL,
				Owner:       kk.Owner,
				Key:         kk.Key,
				FingerPrint: kk.FingerPrint,
				Created:     kk.Created,
				Updated:     kk.Updated,
			}
		}
		log.Printf("Added key key %v\n", kk)
		keyIDs = append(keyIDs, kk.ID)
	}
	return sshKey, keyIDs
}

func (p *packetProvider) getProviderID(provider string) string {
	val, ok := p.indexes[provider]
	if ok {
		val++
	} else {
		val = 1
	}
	p.indexes[provider] = val
	return fmt.Sprintf("%d", val)
}

func (p *packetProvider) CreateCluster(config *config.ClusterProviderConfig, factory k8s.ValidationFactory,
	manager execmanager.ExecutionManager,
	instanceOptions providers.InstanceOptions) (providers.ClusterInstance, error) {
	err := p.ValidateConfig(config)
	if err != nil {
		return nil, err
	}
	p.Lock()
	defer p.Unlock()
	id := fmt.Sprintf("%s-%s", config.Name, p.getProviderID(config.Name))

	root := path.Join(p.root, id)

	clusterInstance := &packetInstance{
		manager:        manager,
		provider:       p,
		root:           root,
		id:             id,
		config:         config,
		configScript:   config.Scripts[configScript],
		installScript:  utils.ParseScript(config.Scripts[installScript]),
		setupScript:    utils.ParseScript(config.Scripts[setupScript]),
		startScript:    utils.ParseScript(config.Scripts[startScript]),
		prepareScript:  utils.ParseScript(config.Scripts[prepareScript]),
		stopScript:     utils.ParseScript(config.Scripts[stopScript]),
		factory:        factory,
		shellInterface: shell.NewManager(manager, id, config, instanceOptions),
		params:         instanceOptions,
		projectID:      os.Getenv(packetProjectID),
		packetAuthKey:  os.Getenv("PACKET_AUTH_TOKEN"),
		devices:        map[string]string{},
	}

	return clusterInstance, nil
}

// CleanupClusters - Cleaning up leaked clusters
func (p *packetProvider) CleanupClusters(ctx context.Context, config *config.ClusterProviderConfig,
	manager execmanager.ExecutionManager, instanceOptions providers.InstanceOptions) {
	if _, ok := config.Scripts[cleanupScript]; !ok {
		// Skip
		return
	}

	logrus.Infof("Starting cleaning up clusters for %s", config.Name)
	shellInterface := shell.NewManager(manager, fmt.Sprintf("%s-cleanup", config.Name), config, instanceOptions)

	p.Lock()
	// Do prepare
	if skipInstall := instanceOptions.NoInstall || p.installDone[config.Name]; !skipInstall {
		if iScript, ok := config.Scripts[installScript]; ok {
			_, err := shellInterface.RunCmd(ctx, "install", utils.ParseScript(iScript), config.Env)
			if err != nil {
				logrus.Warnf("Install command for cluster %s finished with error: %v", config.Name, err)
			} else {
				p.installDone[config.Name] = true
			}
		}
	}
	p.Unlock()

	_, err := shellInterface.RunCmd(ctx, "cleanup", utils.ParseScript(config.Scripts[cleanupScript]), config.Env)
	if err != nil {
		logrus.Warnf("Cleanup command for cluster %s finished with error: %v", config.Name, err)
	}
}

// NewPacketClusterProvider - create new packet provider.
func NewPacketClusterProvider(root string) providers.ClusterProvider {
	utils.ClearFolder(root, true)
	return &packetProvider{
		root:        root,
		clusters:    []packetInstance{},
		indexes:     map[string]int{},
		installDone: map[string]bool{},
	}
}

func (p *packetProvider) ValidateConfig(config *config.ClusterProviderConfig) error {
	if config.Packet == nil {
		return errors.New("packet configuration element should be specified")
	}

	isHardware := len(config.Packet.HardwareDevices) > 0
	isFacility := len(config.Packet.Devices) > 0
	if !isHardware && !isFacility {
		return errors.New("packet configuration devices should be specified")
	}

	if isHardware && len(config.Packet.HardwareReservations) == 0 {
		return errors.New("packet hardware reservations should be specified")
	}

	if isFacility && len(config.Packet.Facilities) == 0 {
		return errors.New("packet configuration facilities should be specified")
	}

	if _, ok := config.Scripts[configScript]; !ok {
		hasKubeConfig := false
		for _, e := range config.Env {
			if strings.HasPrefix(e, "KUBECONFIG=") {
				hasKubeConfig = true
				break
			}
		}
		if !hasKubeConfig {
			return errors.New("invalid config location")
		}
	}
	if _, ok := config.Scripts[startScript]; !ok {
		return errors.New("invalid start script")
	}

	for _, envVar := range config.EnvCheck {
		envValue := os.Getenv(envVar)
		if envValue == "" {
			return errors.Errorf("environment variable are not specified %s Required variables: %v", envValue, config.EnvCheck)
		}
	}

	envValue := os.Getenv("PACKET_AUTH_TOKEN")
	if envValue == "" {
		return errors.New("environment variable are not specified PACKET_AUTH_TOKEN")
	}

	envValue = os.Getenv("PACKET_PROJECT_ID")
	if envValue == "" {
		return errors.New("environment variable are not specified PACKET_PROJECT_ID")
	}

	return nil
}
