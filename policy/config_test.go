package policy

import (
	"encoding/json"
	"io/ioutil"
	"path/filepath"
	"testing"
)

func configFile(t *testing.T, overrides map[string]interface{}) string {
	t.Helper()
	values := map[string]interface{}{
		"PgURL": "postgres://localhost/moemail", "ListenAddress": "127.0.0.1:2555",
		"Hostname": "mail.example.com", "KeyFile": "key.pem", "CertFile": "cert.pem",
	}
	for key, value := range overrides {
		values[key] = value
	}
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(t.TempDir(), "config.json")
	if err := ioutil.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestConfigDefaultsAndTLSModes(t *testing.T) {
	config, err := (&MainConfig{}).Init(configFile(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if config.MXTLSMode != TLSOpportunistic || config.RelayTLSMode != TLSRequired || config.SMTPTimeout != 60 || config.RetryCount != 3 || config.ProcessCount != 3 || config.SmtpHostName != config.Hostname {
		t.Fatalf("bad defaults: %+v", config)
	}
	config, err = (&MainConfig{}).Init(configFile(t, map[string]interface{}{"MXTLSMode": TLSRequired, "RelayTLSMode": TLSOpportunistic}))
	if err != nil || config.MXTLSMode != TLSRequired || config.RelayTLSMode != TLSOpportunistic {
		t.Fatalf("explicit TLS modes not preserved: %v", err)
	}
}

func TestConfigRejectsUnknownOrObsoleteTLSModes(t *testing.T) {
	for _, overrides := range []map[string]interface{}{
		{"MXTLSMode": ""}, {"MXTLSMode": "disabled"}, {"RelayTLSMode": "invalid"},
		{"RequireTLS": true}, {"requiretls": false},
	} {
		if _, err := (&MainConfig{}).Init(configFile(t, overrides)); err == nil {
			t.Fatalf("accepted invalid TLS configuration: %v", overrides)
		}
	}
}

func TestConfigRejectsInvalidLimits(t *testing.T) {
	for _, key := range []string{"RetryCount", "ProcessCount", "SMTPTimeout", "SqlTimeout"} {
		t.Run(key, func(t *testing.T) {
			if _, err := (&MainConfig{}).Init(configFile(t, map[string]interface{}{key: 0})); err == nil {
				t.Fatal("accepted zero limit")
			}
		})
	}
	for _, address := range []string{"localhost", "localhost:0", "localhost:70000"} {
		if _, err := (&MainConfig{}).Init(configFile(t, map[string]interface{}{"ListenAddress": address})); err == nil {
			t.Fatalf("accepted %q", address)
		}
	}
}

func TestQueueExclusiveOwnership(t *testing.T) {
	config, err := (&MainConfig{}).Init(configFile(t, map[string]interface{}{"QueuePath": t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	first, err := (&MainPolicy{MainConfig: *config}).Init(testContext())
	if err != nil {
		t.Fatal(err)
	}
	defer first.db.Close()
	defer first.queueLock.Close()
	if second, err := (&MainPolicy{MainConfig: *config}).Init(testContext()); err == nil {
		second.db.Close()
		second.queueLock.Close()
		t.Fatal("two processes can own the same queue")
	}
}
