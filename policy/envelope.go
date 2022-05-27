package policy

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"os"

	"github.com/flashmob/go-guerrilla/mail"
	uuid "github.com/satori/go.uuid"
)

type MyEnvelope struct {
	mail.Envelope
	IsDSN bool
	UUID  string
	Body  string
}

func (s *MyEnvelope) Save(folderPath string) {
	if s.UUID == "" {
		s.UUID = uuid.NewV4().String()
	}
	s.Body = s.Data.String()
	filePath := folderPath + "/" + s.UUID + ".mail"
	data, err := json.Marshal(s)
	if err != nil {
		log.Printf("Save envelope failed: %s.\n", err.Error())
		return
	}
	err = ioutil.WriteFile(filePath, data, 0600)
	if err != nil {
		log.Printf("Save envelope failed: %s.\n", err.Error())
	}
}

func (s *MyEnvelope) Load(filePath string) (*MyEnvelope, error) {
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		log.Printf("Load envelope failed: %s.\n", err.Error())
		return s, err
	}
	err = json.Unmarshal(data, s)
	if err != nil {
		log.Printf("Load envelope failed: %s.\n", err.Error())
		return s, err
	}
	s.Data.WriteString(s.Body)
	return s, nil
}

func (s *MyEnvelope) Delete(filePath string) {
	os.Remove(filePath)
}
