package policy

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flashmob/go-guerrilla/backends"
	"github.com/flashmob/go-guerrilla/mail"
)

func testEnvelope() *MyEnvelope {
	return &MyEnvelope{UUID: "message", MailFrom: mail.Address{User: "sender", Host: "example.com"}, RcptTo: []mail.Address{{User: "one", Host: "example.net"}, {User: "two", Host: "example.net"}}, Body: "Subject: test\n\noriginal body\n"}
}

func TestProcessorPersistsIndependentSnapshot(t *testing.T) {
	s := MainPolicy{MainConfig: MainConfig{QueuePath: t.TempDir()}, wake: make(chan struct{}, 1)}
	incoming := &mail.Envelope{MailFrom: mail.Address{User: "sender", Host: "example.com"}, RcptTo: []mail.Address{{User: "one", Host: "example.net", ADL: []string{"original"}}}, DeliveryHeader: "Received: test\n"}
	incoming.Data.WriteString("Subject: test\n\noriginal body\n")
	result, err := s.Processor()(backends.DefaultProcessor{}).Process(incoming, backends.TaskSaveMail)
	if err != nil || result.Code() >= 300 {
		t.Fatalf("receive: %v %v", result, err)
	}
	files, err := filepath.Glob(filepath.Join(s.QueuePath, "*.mail"))
	if err != nil || len(files) != 1 {
		t.Fatalf("expected durable file before acceptance: %v %v", files, err)
	}
	incoming.RcptTo[0].ADL[0] = "changed"
	incoming.ResetTransaction()
	incoming.Data.WriteString("Subject: other\n\nother body\n")
	var queued MyEnvelope
	if _, err := queued.Load(files[0]); err != nil {
		t.Fatal(err)
	}
	if queued.Body != "Received: test\nSubject: test\n\noriginal body\n" || queued.RcptTo[0].ADL[0] != "original" {
		t.Fatalf("SMTP reuse corrupted queue: %+v", queued)
	}
}

func TestProcessorRejectsPersistenceFailureAndIgnoresValidation(t *testing.T) {
	s := MainPolicy{MainConfig: MainConfig{QueuePath: filepath.Join(t.TempDir(), "missing")}, wake: make(chan struct{}, 1)}
	incoming := &mail.Envelope{RcptTo: []mail.Address{{User: "one", Host: "example.net"}}}
	processor := s.Processor()(backends.DefaultProcessor{})
	result, err := processor.Process(incoming, backends.TaskSaveMail)
	if err == nil || result.Code() != 451 {
		t.Fatalf("failed save accepted: %v %v", result, err)
	}
	result, err = processor.Process(incoming, backends.TaskValidateRcpt)
	if err != nil || result.Code() >= 300 {
		t.Fatalf("recipient validation tried to save: %v %v", result, err)
	}
}

func TestQueueProgressRoundTrip(t *testing.T) {
	dir := t.TempDir()
	e := testEnvelope()
	if err := e.Save(dir); err != nil {
		t.Fatal(err)
	}
	e.Deliveries[0].Done = true
	e.Deliveries[1].Attempts = 2
	e.Deliveries[1].LastError = "451 temporary"
	if err := e.Save(dir); err != nil {
		t.Fatal(err)
	}
	var loaded MyEnvelope
	if _, err := loaded.Load(filepath.Join(dir, e.UUID+".mail")); err != nil {
		t.Fatal(err)
	}
	if !loaded.Deliveries[0].Done || loaded.Deliveries[1].Done || loaded.Deliveries[1].Attempts != 2 || loaded.Body != e.Body {
		t.Fatalf("progress lost: %+v", loaded)
	}
	info, err := os.Stat(filepath.Join(dir, e.UUID+".mail"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("queue permissions: %v %v", info, err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, ".queue-*.tmp"))
	if len(files) != 0 {
		t.Fatalf("temporary files left: %v", files)
	}
}

func TestLegacyQueueLoadAndCorruptFiles(t *testing.T) {
	dir := t.TempDir()
	legacy := struct {
		mail.Envelope
		UUID  string
		Body  string
		IsDSN bool
	}{Envelope: mail.Envelope{MailFrom: mail.Address{User: "bounce", Host: "catofes.com"}, RcptTo: []mail.Address{{User: "one", Host: "example.net"}}, DeliveryHeader: "Received: legacy\n"}, UUID: "legacy", Body: "Subject: test\n\nbody", IsDSN: true}
	data, err := json.Marshal(&legacy)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "legacy.mail")
	if err := ioutil.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	var e MyEnvelope
	if _, err := e.Load(name); err != nil {
		t.Fatal(err)
	}
	if len(e.Deliveries) != 1 || !e.MailFrom.NullPath || !strings.HasPrefix(e.Body, "Received: legacy\n") {
		t.Fatalf("legacy migration: %+v", e)
	}
	if err := e.Save(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Load(name); err != nil {
		t.Fatal(err)
	}
	if strings.Count(e.Body, "Received: legacy") != 1 {
		t.Fatal("legacy headers duplicated")
	}
	for _, body := range []string{"{", `{"UUID":"other","RcptTo":[]}`, `{"UUID":"legacy","RcptTo":[]}`} {
		if err := ioutil.WriteFile(name, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Load(name); err == nil {
			t.Fatalf("accepted corrupt file %q", body)
		}
	}
}
