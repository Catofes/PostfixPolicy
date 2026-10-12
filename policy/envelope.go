package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/flashmob/go-guerrilla/mail"
	uuid "github.com/satori/go.uuid"
)

// Delivery is the durable progress of one envelope recipient.
type Delivery struct {
	Recipient   mail.Address
	Attempts    int
	NextAttempt time.Time
	LastError   string
	Done        bool
	Held        bool
	Failed      bool
}

// MyEnvelope owns an immutable copy of the message, not the SMTP library's buffers.
// The original field names allow queue files from older versions to be loaded.
type MyEnvelope struct {
	MailFrom       mail.Address
	RcptTo         []mail.Address
	IsDSN          bool
	UUID           string
	Body           string
	DeliveryHeader string `json:",omitempty"` // legacy queue files only
	Deliveries     []Delivery
}

func snapshot(e *mail.Envelope) (*MyEnvelope, error) {
	// Detach the addresses and their nested slices from the SMTP session.
	data, err := json.Marshal(struct {
		MailFrom mail.Address
		RcptTo   []mail.Address
	}{e.MailFrom, e.RcptTo})
	if err != nil {
		return nil, err
	}
	queued := &MyEnvelope{UUID: uuid.NewV4().String(), Body: e.String()}
	if err := json.Unmarshal(data, queued); err != nil {
		return nil, err
	}
	queued.initDeliveries()
	return queued, queued.validate()
}

func (e *MyEnvelope) initDeliveries() {
	if e.Deliveries == nil {
		for _, recipient := range e.RcptTo {
			e.Deliveries = append(e.Deliveries, Delivery{Recipient: recipient})
		}
	}
}

func (e *MyEnvelope) validate() error {
	if e.UUID == "" || strings.Trim(e.UUID, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" {
		return errors.New("invalid queue ID")
	}
	if len(e.RcptTo) == 0 || len(e.Deliveries) != len(e.RcptTo) {
		return errors.New("invalid queue recipients")
	}
	for i, d := range e.Deliveries {
		if d.Recipient.IsEmpty() || d.Recipient.NullPath || d.Recipient.Host == "" || d.Attempts < 0 || d.Recipient.String() != e.RcptTo[i].String() {
			return fmt.Errorf("invalid queue recipient %d", i)
		}
	}
	return nil
}

// Save replaces the queue file only after the complete new contents reach disk.
func (e *MyEnvelope) Save(folderPath string) error {
	if e.UUID == "" {
		e.UUID = uuid.NewV4().String()
	}
	e.initDeliveries()
	if err := e.validate(); err != nil {
		return err
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := ioutil.TempFile(folderPath, ".queue-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), filepath.Join(folderPath, e.UUID+".mail")); err != nil {
		return err
	}
	return syncDirectory(folderPath)
}

func (e *MyEnvelope) Load(filePath string) (*MyEnvelope, error) {
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		return e, err
	}
	var loaded MyEnvelope
	if err := json.Unmarshal(data, &loaded); err != nil {
		return e, err
	}
	if filepath.Base(filePath) != loaded.UUID+".mail" {
		return e, errors.New("queue ID does not match filename")
	}
	loaded.Body = loaded.DeliveryHeader + loaded.Body
	loaded.DeliveryHeader = ""
	if loaded.IsDSN {
		loaded.MailFrom = mail.Address{NullPath: true}
	}
	loaded.initDeliveries()
	if err := loaded.validate(); err != nil {
		return e, err
	}
	*e = loaded
	return e, nil
}

func (e *MyEnvelope) Delete(folderPath string) error {
	if err := os.Remove(filepath.Join(folderPath, e.UUID+".mail")); err != nil {
		return err
	}
	return syncDirectory(folderPath)
}

func syncDirectory(folderPath string) error {
	dir, err := os.Open(folderPath)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
