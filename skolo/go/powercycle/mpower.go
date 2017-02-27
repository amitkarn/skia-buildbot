package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"go.skia.org/infra/go/sklog"
	"go.skia.org/infra/go/util"
	"golang.org/x/crypto/ssh"
)

// TODO(stephana): Replace the hard coded configuration with a way
// to parse configuration files that describe the devices attached
// to the mPower strip.
var mPowerConfig = &MPowerConfig{
	address: "192.168.2.212:22",
	user:    "ubnt",
	devPortMap: map[string]int{
		"skia-e-linux-001": 1,
		"skia-e-linux-002": 2,
		"skia-e-linux-003": 3,
		"skia-e-linux-004": 4,
		"test-relay":       5,
		"not-used-6":       6,
		"not-used-7":       7,
		"not-used-8":       8,
	},
}

// MPowerConfig contains the necessary parameters to connect
// and control an mPowerPro power strip.
type MPowerConfig struct {
	address    string         // IP address and port of the device, i.e. 192.168.1.33:22
	user       string         // User of the ssh connection
	devPortMap map[string]int // Mapping between device name and port on the power strip.
}

// Constants used to access the Ubiquiti mPower Pro.
const (
	// Location of the directory where files are that control the device.
	MPOWER_ROOT = "/proc/power"

	// String template to address a relay.
	MPOWER_RELAY = "relay%d"

	// Values to write to the relay file to disable/enable ports.
	PORT_OFF = 0
	PORT_ON  = 1
)

// Mapping between strings and port states.
var POWER_VALUES = map[string]int{
	"0": PORT_OFF,
	"1": PORT_ON,
}

// MPowerClient implements the DeviceGroup interface.
type MPowerClient struct {
	client    *ssh.Client
	deviceIDs []string
}

// NewMPowerClient returns a new instance of DeviceGroup for the
// mPowerPro power strip.
func NewMPowerClient() (DeviceGroup, error) {
	key, err := ioutil.ReadFile(os.ExpandEnv("${HOME}/.ssh/id_rsa"))
	if err != nil {
		return nil, fmt.Errorf("Unable to read private key: %v", err)
	}
	sklog.Infof("Retrieved private key")

	// Create the Signer for this private key.
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("Unable to parse private key: %v", err)
	}
	sklog.Infof("Parsed private key")

	sshConfig := &ssh.ClientConfig{
		User: mPowerConfig.user,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		Config: ssh.Config{
			Ciphers: []string{"aes128-cbc", "3des-cbc", "aes256-cbc",
				"twofish256-cbc", "twofish-cbc", "twofish128-cbc", "blowfish-cbc"},
		},
	}

	sklog.Infof("Signed private key")

	client, err := ssh.Dial("tcp", mPowerConfig.address, sshConfig)
	if err != nil {
		return nil, err
	}

	sklog.Infof("Dial successful")

	devIDs := make([]string, 0, len(mPowerConfig.devPortMap))
	for id := range mPowerConfig.devPortMap {
		devIDs = append(devIDs, id)
	}
	sort.Strings(devIDs)

	ret := &MPowerClient{
		client:    client,
		deviceIDs: devIDs,
	}

	if err := ret.ping(); err != nil {
		return nil, err
	}

	return ret, nil
}

// DeviceIDs, see DeviceGroup interface.
func (m *MPowerClient) DeviceIDs() []string {
	return m.deviceIDs
}

// PowerCycle, see PowerCycle interface.
func (m *MPowerClient) PowerCycle(devID string) error {
	if !util.In(devID, m.deviceIDs) {
		return fmt.Errorf("Unknown device ID: %s", devID)
	}

	port := mPowerConfig.devPortMap[devID]
	if err := m.setPortValue(port, PORT_OFF); err != nil {
		return err
	}

	sklog.Infof("Switched port %d off. Waiting for 10 seconds.", port)
	time.Sleep(10 * time.Second)
	if err := m.setPortValue(port, PORT_ON); err != nil {
		return err
	}

	sklog.Infof("Switched port %d on.", port)
	return nil
}

// ping issues a command to the device to verify that the
// connection works.
func (m *MPowerClient) ping() error {
	sklog.Infof("Executing ping.")

	session, err := m.client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	out, err := session.CombinedOutput("pwd")
	if err != nil {
		return err
	}
	sklog.Infof("PWD: %s", string(out))
	return nil
}

// getPortValue returns the status of given port.
func (m *MPowerClient) getPortValue(port int) (int, error) {
	if !m.validPort(port) {
		return PORT_OFF, fmt.Errorf("Invalid port. Expected 1-8 got %d", port)
	}

	session, err := m.client.NewSession()
	if err != nil {
		return PORT_OFF, err
	}
	defer session.Close()

	cmd := fmt.Sprintf("cat %s", m.getRelayFile(port))
	outBytes, err := session.CombinedOutput(cmd)
	if err != nil {
		return PORT_OFF, err
	}
	out := strings.TrimSpace(string(outBytes))
	current, ok := POWER_VALUES[out]
	if !ok {
		return PORT_OFF, fmt.Errorf("Got unexpected relay value: %s", out)
	}
	return current, nil
}

// getRelayFile returns name of the relay file for the given port.
func (m *MPowerClient) getRelayFile(port int) string {
	return path.Join(MPOWER_ROOT, fmt.Sprintf(MPOWER_RELAY, port))
}

// validPort returns true if the given port is valid.
func (m *MPowerClient) validPort(port int) bool {
	return (port >= 1) && (port <= 8)
}

// setPortValue sets the value of the given port to the given value.
func (m *MPowerClient) setPortValue(port int, newVal int) error {
	if !m.validPort(port) {
		return fmt.Errorf("Invalid port. Expected 1-8 got %d", port)
	}

	session, err := m.client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	// Check if the value is already the target value.
	if current, err := m.getPortValue(port); err != nil {
		return err
	} else if current == newVal {
		return nil
	}

	cmd := fmt.Sprintf("echo '%d' > %s", newVal, m.getRelayFile(port))
	sklog.Infof("Executing: %s", cmd)
	_, err = session.CombinedOutput(cmd)
	if err != nil {
		return err
	}

	// Check if the value was set correctly.
	if current, err := m.getPortValue(port); err != nil {
		return err
	} else if current != newVal {
		return fmt.Errorf("Could not read back new value. Got %d", current)
	}

	return nil
}