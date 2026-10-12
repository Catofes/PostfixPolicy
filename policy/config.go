package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"strconv"
	"strings"
)

const (
	TLSOpportunistic = "opportunistic"
	TLSRequired      = "required"
)

type MainConfig struct {
	ListenAddress string
	PgURL         string
	ServerMark    string
	RegionMark    string
	DefaultMark   string
	Hostname      string
	KeyFile       string
	CertFile      string
	SqlTimeout    int
	RetryCount    int
	QueuePath     string
	SmtpHostName  string
	ProcessCount  int
	SMTPTimeout   int
	MXTLSMode     string
	RelayTLSMode  string
	TLSCAFile     string
}

func (s *MainConfig) Init(path string) (*MainConfig, error) {
	*s = MainConfig{SqlTimeout: 30, RetryCount: 3, ProcessCount: 3, QueuePath: ".", SMTPTimeout: 60, MXTLSMode: TLSOpportunistic, RelayTLSMode: TLSRequired}
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	for field := range fields {
		if strings.EqualFold(field, "RequireTLS") {
			return nil, errors.New("RequireTLS has been replaced by MXTLSMode and RelayTLSMode")
		}
	}
	if s.SmtpHostName == "" {
		s.SmtpHostName = s.Hostname
	}
	return s, s.validate()
}

func (s *MainConfig) validate() error {
	if s.MXTLSMode != TLSOpportunistic && s.MXTLSMode != TLSRequired {
		return errors.New("MXTLSMode must be opportunistic or required")
	}
	if s.RelayTLSMode != TLSOpportunistic && s.RelayTLSMode != TLSRequired {
		return errors.New("RelayTLSMode must be opportunistic or required")
	}
	if s.SqlTimeout <= 0 || s.RetryCount <= 0 || s.ProcessCount <= 0 || s.SMTPTimeout <= 0 {
		return errors.New("SqlTimeout, RetryCount, ProcessCount and SMTPTimeout must be positive")
	}
	if s.QueuePath == "" || s.PgURL == "" || s.Hostname == "" || s.SmtpHostName == "" || strings.ContainsAny(s.Hostname+s.SmtpHostName, "\r\n @") {
		return errors.New("QueuePath, PgURL, Hostname and SmtpHostName must be configured correctly")
	}
	_, port, err := net.SplitHostPort(s.ListenAddress)
	if err != nil {
		return errors.New("ListenAddress must contain a host and port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("ListenAddress has an invalid port")
	}
	if s.KeyFile == "" || s.CertFile == "" {
		return errors.New("KeyFile and CertFile are required for incoming STARTTLS")
	}
	return nil
}
