package server

import (
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"time"

	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes/timestamp"
	"github.com/lf-edge/eve/api/go/config"
	"github.com/lf-edge/eve/api/go/info"
	"github.com/lf-edge/eve/api/go/logs"
	"github.com/lf-edge/eve/api/go/metrics"
	uuid "github.com/satori/go.uuid"
)

const (
	deviceCertFilename    = "device-certificate.pem"
	deviceOnboardFilename = "onboard-certificate.pem"
	deviceSerialFilename  = "serial.txt"
	onboardCertFilename   = "cert.pem"
	onboardCertSerials    = "onboard-serials.txt"
	logDir                = "logs"
	metricsDir            = "metrics"
	infoDir               = "info"
	configPath            = "config.json"
)

type deviceManagerFile struct {
	devicePath   string
	onboardPath  string
	cacheTimeout int
	lastUpdate   time.Time
	// thse are for caching only
	onboardCerts map[string]map[string]bool
	deviceCerts  map[string]uuid.UUID
	devices      map[uuid.UUID]deviceStorage
}

// SetCacheTimeout set the timeout for refreshing the cache, unused in memory
func (d *deviceManagerFile) SetCacheTimeout(timeout int) {
	d.cacheTimeout = timeout
}

// CheckOnboardCert see if a particular certificate and serial combination is valid
func (d *deviceManagerFile) CheckOnboardCert(cert *x509.Certificate, serial string) (bool, error) {
	// do not accept a nil certificate
	if cert == nil {
		return false, fmt.Errorf("invalid nil certificate")
	}
	// refresh certs from filesystem, if needed - includes checking if necessary based on timer
	err := d.refreshCerts()
	if err != nil {
		return false, fmt.Errorf("unable to refresh certs from filesystem: %v", err)
	}
	if !d.checkValidOnboardSerial(cert, serial) {
		return false, nil
	}
	if d.getOnboardSerialDevice(cert, serial) != nil {
		return false, nil
	}
	return true, nil
}

// CheckDeviceCert see if a particular certificate is a valid registered device certificate
func (d *deviceManagerFile) CheckDeviceCert(cert *x509.Certificate) (*uuid.UUID, error) {
	if cert == nil {
		return nil, fmt.Errorf("invalid nil certificate")
	}
	// refresh certs from filesystem, if needed - includes checking if necessary based on timer
	err := d.refreshCerts()
	if err != nil {
		return nil, fmt.Errorf("unable to refresh certs from filesystem: %v", err)
	}
	certStr := string(cert.Raw)
	if u, ok := d.deviceCerts[certStr]; ok {
		return &u, nil
	}
	return nil, nil
}

// RegisterDeviceCert register a new device cert
func (d *deviceManagerFile) RegisterDeviceCert(cert, onboard *x509.Certificate, serial string) (*uuid.UUID, error) {
	// refresh certs from filesystem, if needed - includes checking if necessary based on timer
	err := d.refreshCerts()
	if err != nil {
		return nil, fmt.Errorf("unable to refresh certs from filesystem: %v", err)
	}
	// check if it already exists - this also checks for nil cert
	u, err := d.CheckDeviceCert(cert)
	if err != nil {
		return nil, err
	}
	// if we found a uuid, then it already exists
	if u != nil {
		return nil, fmt.Errorf("device already registered")
	}
	// generate a new uuid
	unew, err := uuid.NewV4()
	if err != nil {
		return nil, fmt.Errorf("error generating uuid for device: %v", err)
	}

	// create filesystem tree and subdirs for the new device
	devicePath := d.getDevicePath(unew)
	err = os.MkdirAll(devicePath, 0755)
	if err != nil {
		return nil, fmt.Errorf("error creating new device tree %s: %v", devicePath, err)
	}

	// save the device certificate
	certPath := path.Join(devicePath, deviceCertFilename)
	err = ioutil.WriteFile(certPath, cert.Raw, 0644)
	if err != nil {
		return nil, fmt.Errorf("error saving device certificate to %s: %v", certPath, err)
	}

	// save the onboard certificate and serial
	certPath = path.Join(devicePath, deviceOnboardFilename)
	err = ioutil.WriteFile(certPath, onboard.Raw, 0644)
	if err != nil {
		return nil, fmt.Errorf("error saving device onboard certificate to %s: %v", certPath, err)
	}
	serialPath := path.Join(devicePath, deviceSerialFilename)
	err = ioutil.WriteFile(serialPath, []byte(serial), 0644)
	if err != nil {
		return nil, fmt.Errorf("error saving device serial to %s: %v", serialPath, err)
	}

	// create the necessary directories
	for _, p := range []string{logDir, metricsDir, infoDir} {
		cur := path.Join(devicePath, p)
		err = os.MkdirAll(cur, 0755)
		if err != nil {
			return nil, fmt.Errorf("error creating new device sub-path %s: %v", cur, err)
		}
	}

	// save new one to cache
	d.deviceCerts[string(cert.Raw)] = unew
	d.devices[unew] = deviceStorage{
		onboard: onboard,
		serial:  serial,
	}

	return &unew, nil
}

// WriteInfo write an info message
func (d *deviceManagerFile) WriteInfo(m *info.ZInfoMsg) error {
	// make sure it is not nil
	if m == nil {
		return fmt.Errorf("invalid nil message")
	}
	// get the uuid
	u, err := uuid.FromString(m.DevId)
	if err != nil {
		return fmt.Errorf("unable to retrieve valid device UUID from message as %s: %v", m.DevId, err)
	}
	// check that the device actually exists
	if _, ok := d.devices[u]; !ok {
		return fmt.Errorf("unregistered device uuid: %s", m.DevId)
	}
	err = d.writeProtobufToJsonFile(u, infoDir, m.AtTimeStamp, m)
	if err != nil {
		return fmt.Errorf("failed to write info to file: %v", err)
	}
	return nil
}

// WriteLogs write a message of logs
func (d *deviceManagerFile) WriteLogs(m *logs.LogBundle) error {
	// make sure it is not nil
	if m == nil {
		return fmt.Errorf("invalid nil message")
	}
	// get the uuid
	u, err := uuid.FromString(m.DevID)
	if err != nil {
		return fmt.Errorf("unable to retrieve valid device UUID from message as %s: %v", m.DevID, err)
	}
	// check that the device actually exists
	if _, ok := d.devices[u]; !ok {
		return fmt.Errorf("unregistered device uuid: %s", m.DevID)
	}
	err = d.writeProtobufToJsonFile(u, logDir, m.Timestamp, m)
	if err != nil {
		return fmt.Errorf("failed to write logs to file: %v", err)
	}
	return nil
}

// WriteMetrics write a metrics message
func (d *deviceManagerFile) WriteMetrics(m *metrics.ZMetricMsg) error {
	// make sure it is not nil
	if m == nil {
		return fmt.Errorf("invalid nil message")
	}
	// get the uuid
	u, err := uuid.FromString(m.DevID)
	if err != nil {
		return fmt.Errorf("unable to retrieve valid device UUID from message as %s: %v", m.DevID, err)
	}
	// check that the device actually exists
	if _, ok := d.devices[u]; !ok {
		return fmt.Errorf("unregistered device uuid: %s", m.DevID)
	}
	err = d.writeProtobufToJsonFile(u, metricsDir, m.AtTimeStamp, m)
	if err != nil {
		return fmt.Errorf("failed to write metrics to file: %v", err)
	}
	return nil
}

// GetConfig retrieve the config for a particular device
func (d *deviceManagerFile) GetConfig(u uuid.UUID) (*config.EdgeDevConfig, error) {
	// read the config from disk
	fullConfigPath := path.Join(d.getDevicePath(u), configPath)
	b, err := ioutil.ReadFile(fullConfigPath)
	if err != nil {
		return nil, fmt.Errorf("could not read config from %s: %v", fullConfigPath, err)
	}
	// convert it to the message format
	msg := &config.EdgeDevConfig{}
	err = jsonpb.UnmarshalString(string(b), msg)
	if err != nil {
		return nil, fmt.Errorf("error parsing the config to protobuf: %v", err)
	}
	return msg, nil
}

// refreshCerts refresh certs from disk
func (d *deviceManagerFile) refreshCerts() error {
	// is it time to update the cache again?
	now := time.Now()
	if now.Sub(d.lastUpdate).Seconds() < float64(d.cacheTimeout) {
		return nil
	}

	// create new vars to hold while we load
	onboardCerts := make(map[string]map[string]bool)
	deviceCerts := make(map[string]uuid.UUID)
	devices := make(map[uuid.UUID]deviceStorage)

	// scan the onboard path for all files which end in ".pem" and load them
	candidates, err := ioutil.ReadDir(d.onboardPath)
	if err != nil {
		return fmt.Errorf("unable to read onboarding certificates at %s: %v", d.onboardPath, err)
	}
	// check each file to make sure it is an onboarding cert
	for _, fi := range candidates {
		// we only are interested in directories
		if !fi.IsDir() {
			continue
		}
		name := fi.Name()
		f := path.Join(d.onboardPath, name, onboardCertFilename)
		_, err = os.Stat(f)
		// if we cannot list the file, we do not care why, just continue
		if err != nil {
			continue
		}

		// read the file
		b, err := ioutil.ReadFile(f)
		if err != nil {
			return fmt.Errorf("unable to read onboard certificate file %s: %v", f, err)
		}
		// convert into a certificate
		cert, err := x509.ParseCertificate(b)
		if err != nil {
			return fmt.Errorf("unable to convert data from file %s to onboard certificate: %v", f, err)
		}
		certStr := string(cert.Raw)
		onboardCerts[certStr] = make(map[string]bool)

		// get the serial list
		f = path.Join(d.onboardPath, name, onboardCertSerials)
		_, err = os.Stat(f)
		// if we cannot list the file, we do not care why, just continue
		if err != nil {
			continue
		}
		b, err = ioutil.ReadFile(f)
		if err != nil {
			return fmt.Errorf("unable to read onboard serial file %s: %v", f, err)
		}
		// convert the []byte to string, split and save
		for _, serial := range strings.Fields(string(b)) {
			onboardCerts[certStr][serial] = true
		}
	}
	// replace the existing onboard certificates
	d.onboardCerts = onboardCerts

	// scan the device path for each dir which is the UUID
	//   and in each one, if a cert exists with the appropriate name, load it
	candidates, err = ioutil.ReadDir(d.devicePath)
	if err != nil {
		return fmt.Errorf("unable to read devices at %s: %v", d.devicePath, err)
	}
	// check each directory to see if it is a valid device directory
	for _, fi := range candidates {
		// we only are interested in directories
		if !fi.IsDir() {
			continue
		}
		name := fi.Name()
		// convert the path name to a UUID
		u, err := uuid.FromString(name)
		if err != nil {
			return fmt.Errorf("unable to convert device uuid from directory name %s: %v", name, err)
		}

		// load the device certificate
		f := path.Join(d.devicePath, name, deviceCertFilename)
		_, err = os.Stat(f)
		// if we cannot list the file, we do not care why, just continue
		if err != nil {
			continue
		}
		// read the file
		b, err := ioutil.ReadFile(f)
		if err != nil {
			return fmt.Errorf("unable to read device certificate file %s: %v", f, err)
		}
		// convert into a certificate
		cert, err := x509.ParseCertificate(b)
		if err != nil {
			return fmt.Errorf("unable to convert data from file %s to device certificate: %v", f, err)
		}
		certStr := string(cert.Raw)
		deviceCerts[certStr] = u

		// load the device onboarding certificate and serial
		f = path.Join(d.devicePath, name, deviceOnboardFilename)
		_, err = os.Stat(f)
		// if we cannot list the file, we do not care why, just continue
		if err != nil {
			continue
		}
		// read the file
		b, err = ioutil.ReadFile(f)
		if err != nil {
			return fmt.Errorf("unable to read device onboard certificate file %s: %v", f, err)
		}
		// convert into a certificate
		cert, err = x509.ParseCertificate(b)
		if err != nil {
			return fmt.Errorf("unable to convert data from file %s to device onboard certificate: %v", f, err)
		}
		certStr = string(cert.Raw)
		if err != nil {
			return fmt.Errorf("unable to convert device uuid from directory name %s: %v", name, err)
		}
		// and the serial
		f = path.Join(d.devicePath, name, deviceSerialFilename)
		_, err = os.Stat(f)
		// if we cannot list the file, we do not care why, just continue
		if err != nil {
			continue
		}
		// read the file
		b, err = ioutil.ReadFile(f)
		if err != nil {
			return fmt.Errorf("unable to read device serial file %s: %v", f, err)
		}
		devices[u] = deviceStorage{
			onboard: cert,
			serial:  string(b),
		}
	}
	// replace the existing device certificates
	d.deviceCerts = deviceCerts
	// replace the existing device cache
	d.devices = devices

	// mark the time we updated
	d.lastUpdate = now
	return nil
}

// getDevicePath get the path for a given device
func (d *deviceManagerFile) getDevicePath(u uuid.UUID) string {
	return path.Join(d.devicePath, u.String())
}

// writeProtobufToJsonFile write a protobuf to a timestamped file in the given directory
func (d *deviceManagerFile) writeProtobufToJsonFile(u uuid.UUID, dir string, ts *timestamp.Timestamp, msg proto.Message) error {
	filename := ts.String()
	fullPath := path.Join(d.getDevicePath(u), dir, filename)
	f, err := os.Create(fullPath)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %v", fullPath, err)
	}
	defer f.Close()
	mler := jsonpb.Marshaler{}
	err = mler.Marshal(f, msg)
	if err != nil {
		return fmt.Errorf("failed to marshal protobuf message into json: %v", err)
	}
	// no need to f.Close() as it happens automatically
	return nil
}

// checkValidOnboardSerial see if a particular certificate+serial combinaton is valid
// does **not** check if it has been used
func (d *deviceManagerFile) checkValidOnboardSerial(cert *x509.Certificate, serial string) bool {
	certStr := string(cert.Raw)
	if c, ok := d.onboardCerts[certStr]; ok {
		// accept the specific serial or the wildcard
		if _, ok := c[serial]; ok {
			return true
		}
		if _, ok := c["*"]; ok {
			return true
		}
	}
	return false
}

// getOnboardSerialDevice see if a particular certificate+serial combinaton has been used and get its device uuid
func (d *deviceManagerFile) getOnboardSerialDevice(cert *x509.Certificate, serial string) *uuid.UUID {
	certStr := string(cert.Raw)
	for uid, dev := range d.devices {
		dCertStr := string(dev.onboard.Raw)
		if dCertStr == certStr && serial == dev.serial {
			return &uid
		}
	}
	return nil
}
