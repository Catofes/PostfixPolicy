package policy

import (
	"encoding/json"
	"io/ioutil"
	"log"
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
}

func (s *MainConfig) Init(path string) *MainConfig {
	s.SqlTimeout = 30
	s.RetryCount = 3
	s.ProcessCount = 3
	s.QueuePath = "."
	data, err := ioutil.ReadFile(path)
	if err != nil {
		log.Fatal("[Fatal] Can not read config file.")
	}
	if err := json.Unmarshal(data, s); err != nil {
		log.Fatal("[Fatal] Can not parse config file.")
	}
	return s
}
