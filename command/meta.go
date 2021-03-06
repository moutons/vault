package command

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/command/token"
	"github.com/mitchellh/cli"
)

// EnvVaultAddress can be used to set the address of Vault
const EnvVaultAddress = "VAULT_ADDR"

// FlagSetFlags is an enum to define what flags are present in the
// default FlagSet returned by Meta.FlagSet.
type FlagSetFlags uint

const (
	FlagSetNone    FlagSetFlags = 0
	FlagSetServer  FlagSetFlags = 1 << iota
	FlagSetDefault              = FlagSetServer
)

// Meta contains the meta-options and functionality that nearly every
// Vault command inherits.
type Meta struct {
	ClientToken string
	Ui          cli.Ui

	// The things below can be set, but aren't common
	ForceAddress string  // Address to force for API clients
	ForceConfig  *Config // Force a config, don't load from disk

	// These are set by the command line flags.
	flagAddress  string
	flagCACert   string
	flagCAPath   string
	flagInsecure bool

	// These are internal and shouldn't be modified or access by anyone
	// except Meta.
	config *Config
}

// Client returns the API client to a Vault server given the configured
// flag settings for this command.
func (m *Meta) Client() (*api.Client, error) {
	config := api.DefaultConfig()
	if v := os.Getenv(EnvVaultAddress); v != "" {
		config.Address = v
	}
	if m.flagAddress != "" {
		config.Address = m.flagAddress
	}
	if m.ForceAddress != "" {
		config.Address = m.ForceAddress
	}

	// If we need custom TLS configuration, then set it
	if m.flagCACert != "" || m.flagCAPath != "" || m.flagInsecure {
		var certPool *x509.CertPool
		var err error
		if m.flagCACert != "" {
			certPool, err = m.loadCACert(m.flagCACert)
		} else if m.flagCAPath != "" {
			certPool, err = m.loadCAPath(m.flagCAPath)
		}
		if err != nil {
			return nil, fmt.Errorf("Error setting up CA path: %s", err)
		}

		tlsConfig := &tls.Config{
			InsecureSkipVerify: m.flagInsecure,
			MinVersion:         tls.VersionTLS12,
			RootCAs:            certPool,
		}

		client := *http.DefaultClient
		client.Transport = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			Dial: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).Dial,
			TLSClientConfig:     tlsConfig,
			TLSHandshakeTimeout: 10 * time.Second,
		}

		config.HttpClient = &client
	}

	// Build the client
	client, err := api.NewClient(config)
	if err != nil {
		return nil, err
	}

	// If we have a token directly, then set that
	token := m.ClientToken

	// If we don't have a token, check the token helper
	if token == "" {
		// If we have a token, then set that
		tokenHelper, err := m.TokenHelper()
		if err != nil {
			return nil, err
		}
		token, err = tokenHelper.Get()
		if err != nil {
			return nil, err
		}
	}

	// Set the token
	if token != "" {
		client.SetToken(token)
	}

	return client, nil
}

// Config loads the configuration and returns it. If the configuration
// is already loaded, it is returned.
func (m *Meta) Config() (*Config, error) {
	if m.config != nil {
		return m.config, nil
	}
	if m.ForceConfig != nil {
		return m.ForceConfig, nil
	}

	var err error
	m.config, err = LoadConfig("")
	if err != nil {
		return nil, err
	}

	return m.config, nil
}

// FlagSet returns a FlagSet with the common flags that every
// command implements. The exact behavior of FlagSet can be configured
// using the flags as the second parameter, for example to disable
// server settings on the commands that don't talk to a server.
func (m *Meta) FlagSet(n string, fs FlagSetFlags) *flag.FlagSet {
	f := flag.NewFlagSet(n, flag.ContinueOnError)

	// FlagSetServer tells us to enable the settings for selecting
	// the server information.
	if fs&FlagSetServer != 0 {
		f.StringVar(&m.flagAddress, "address", "", "")
		f.StringVar(&m.flagCACert, "ca-cert", "", "")
		f.StringVar(&m.flagCAPath, "ca-path", "", "")
		f.BoolVar(&m.flagInsecure, "insecure", false, "")
	}

	// Create an io.Writer that writes to our Ui properly for errors.
	// This is kind of a hack, but it does the job. Basically: create
	// a pipe, use a scanner to break it into lines, and output each line
	// to the UI. Do this forever.
	errR, errW := io.Pipe()
	errScanner := bufio.NewScanner(errR)
	go func() {
		for errScanner.Scan() {
			m.Ui.Error(errScanner.Text())
		}
	}()
	f.SetOutput(errW)

	return f
}

// TokenHelper returns the token helper that is configured for Vault.
func (m *Meta) TokenHelper() (*token.Helper, error) {
	config, err := m.Config()
	if err != nil {
		return nil, err
	}

	path := config.TokenHelper
	if path == "" {
		path = "disk"
	}

	path = token.HelperPath(path)
	return &token.Helper{Path: path}, nil
}

func (m *Meta) loadCACert(path string) (*x509.CertPool, error) {
	certs, err := m.loadCertFromPEM(path)
	if err != nil {
		return nil, fmt.Errorf("Error loading %s: %s", path, err)
	}

	result := x509.NewCertPool()
	for _, cert := range certs {
		result.AddCert(cert)
	}

	return result, nil
}

func (m *Meta) loadCAPath(path string) (*x509.CertPool, error) {
	result := x509.NewCertPool()
	fn := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		certs, err := m.loadCertFromPEM(path)
		if err != nil {
			return fmt.Errorf("Error loading %s: %s", path, err)
		}

		for _, cert := range certs {
			result.AddCert(cert)
		}
		return nil
	}

	return result, filepath.Walk(path, fn)
}

func (m *Meta) loadCertFromPEM(path string) ([]*x509.Certificate, error) {
	pemCerts, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	certs := make([]*x509.Certificate, 0, 5)
	for len(pemCerts) > 0 {
		var block *pem.Block
		block, pemCerts = pem.Decode(pemCerts)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			continue
		}

		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}

		certs = append(certs, cert)
	}

	return certs, nil
}
