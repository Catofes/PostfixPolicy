package policy

import (
	"net"
	"log"
	"context"
	"database/sql"
	_ "github.com/lib/pq"
	"github.com/Catofes/go-guerrilla"
	"github.com/flashmob/go-guerrilla/backends"
	"github.com/flashmob/go-guerrilla/mail"
	"strings"
	"time"
	"net/url"
	"net/smtp"
	"os"
	"crypto/tls"
	"errors"
	"sort"
	"github.com/satori/go.uuid"
	"gopkg.in/gomail.v2"
	"regexp"
	"fmt"
	"io/ioutil"
	"path"
	"net/textproto"
)

type MainPolicy struct {
	MainConfig
	ln        net.Listener
	ctx       context.Context
	db        *sql.DB
	daemon    *guerrilla.Daemon
	mailQueue chan MyEnvelope
	lock      chan bool
}

func (s *MainPolicy) Processor() backends.Decorator {
	return func(p backends.Processor) backends.Processor {
		return backends.ProcessWith(
			func(e *mail.Envelope, task backends.SelectTask) (backends.Result, error) {
				s.mailQueue <- MyEnvelope{Envelope: *e}
				return p.Process(e, task)
			},
		)
	}
}

func (s *MainPolicy) Init(ctx context.Context) *MainPolicy {
	var err error
	s.ctx = ctx
	f, err := os.Stat(s.QueuePath)
	if err != nil {
		log.Fatal(err)
	}
	s.lock = make(chan bool, s.ProcessCount)
	if !f.Mode().IsDir() {
		log.Fatal("Queue Path is not a folder.")
	}
	s.db, err = sql.Open("postgres", s.PgURL)
	s.mailQueue = make(chan MyEnvelope)
	if err != nil {
		log.Fatal(err)
	}
	config := guerrilla.AppConfig{
		LogFile:  "stdout",
		LogLevel: "info",
	}
	config.BackendConfig = backends.BackendConfig{
		"save_workers_size":  1,
		"save_process":       "HeadersParser|Header|Hasher|Send",
		"log_received_mails": true,
	}
	config.Servers = append(config.Servers,
		guerrilla.ServerConfig{
			IsEnabled:          true,
			IgnoreAllowedHosts: true,
			Hostname:           s.Hostname,
			MaxSize:            100 * 1024 * 1024,
			ListenInterface:    s.ListenAddress,
			PrivateKeyFile:     s.KeyFile,
			PublicKeyFile:      s.CertFile,
		}, )

	s.daemon = &guerrilla.Daemon{Config: &config}
	s.daemon.AddProcessor("Send", s.Processor)
	return s
}

func (s *MainPolicy) loop() {
	go func() {
		for {
			select {
			case envelope := <-s.mailQueue:
				go s.handle(envelope)
			case <-s.ctx.Done():
				return
			}
		}
	}()
}

func (s *MainPolicy) borrow() {
	s.lock <- true
}

func (s *MainPolicy) repay() {
	go func() {
		<-s.lock
	}()
}

func (s *MainPolicy) handle(e MyEnvelope) {
	e.Save(s.QueuePath)
	getRcpt := func(input []mail.Address) string {
		str := ""
		for _, v := range input {
			str += v.User + "@" + v.Host + " "
		}
		return str
	}
	log.Printf("[%s] Received message from %s to %s\n", e.UUID, e.MailFrom.User+"@"+e.MailFrom.Host, getRcpt(e.RcptTo))
	from := e.MailFrom
	if e.IsDSN {
		go func(e MyEnvelope) {
			s.sendMail(e, e.RcptTo[0], &url.URL{Scheme: "default"})
		}(e)
		return
	}
	for _, to := range e.RcptTo {
		go func(e MyEnvelope, to mail.Address) {
			nexthop, err := s.getDestination(from, to)
			if err != nil {
				s.returnMail(e, to, err)
				return
			}
			s.sendMail(e, to, nexthop)
		}(e, to)
	}
}

func (s *MainPolicy) parseAddress(from, to mail.Address) (a, b []string) {
	a = make([]string, 0)
	b = make([]string, 0)
	a = append(a, from.User+"@"+from.Host)
	b = append(b, to.User+"@"+to.Host)
	if v := strings.Split(from.User, "+"); len(v) == 2 {
		a = append(a, v[0]+"@"+from.Host)
	}
	if v := strings.Split(to.User, "+"); len(v) == 2 {
		b = append(b, v[0]+"@"+to.Host)
	}
	a = append(a, from.Host)
	b = append(b, to.Host)
	v := strings.Split(from.Host, ".")
	for i := 0; i+1 < len(v); i++ {
		a = append(a, "."+strings.Join(v[i+1:], "."))
	}
	v = strings.Split(to.Host, ".")
	for i := 0; i+1 < len(v); i++ {
		b = append(b, "."+strings.Join(v[i+1:], "."))
	}
	a = append(a, "*")
	b = append(b, "*")
	return
}

func (s *MainPolicy) queryDatabase(from, to []string) string {
	ctx, cancel := context.WithTimeout(s.ctx, time.Duration(s.SqlTimeout)*time.Second)
	defer cancel()
	for _, f := range from {
		for _, t := range to {
			var result string
			err := s.db.QueryRowContext(ctx, "SELECT destination FROM transport_senders "+
				"WHERE source=$1 AND recipient=$2 AND (region=$3 OR region=$4 OR region=$5) ORDER BY region DESC LIMIT 1",
				f, t, s.ServerMark, s.RegionMark, s.DefaultMark).Scan(&result)
			if err != nil {
				continue
			}
			return result
		}
	}
	return "default://"
}

func (s *MainPolicy) getDestination(from, to mail.Address) (*url.URL, error) {
	f, t := s.parseAddress(from, to)
	destination := s.queryDatabase(f, t)
	result, err := (&url.URL{}).Parse(destination)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *MainPolicy) returnMail(e MyEnvelope, to mail.Address, err error) {
	re := MyEnvelope{}
	re.UUID = uuid.NewV4().String()
	re.MailFrom = mail.Address{"bounce", "catofes.com"}
	re.RcptTo = append(re.RcptTo, e.MailFrom)
	re.Subject = "Mail Delivery System "
	re.IsDSN = true

	m := gomail.NewMessage()
	m.SetAddressHeader("From", "bounce@catofes.com", "bounce")
	var myExp = regexp.MustCompile(`^[^<]*<([a-zA-Z0-9_\.\+@\-]+)>$`)
	m.SetAddressHeader("To", e.MailFrom.User+"@"+e.MailFrom.Host, "bounce")
	if v := e.Header.Get("From"); v != "" {
		if s := myExp.FindSubmatch([]byte(v)); s != nil {
			if len(s) > 1 {
				m.SetAddressHeader("To", string(s[1]), "")
			}
		}
	}
	if v := e.Header.Get("Return-path"); v != "" {
		if s := myExp.FindSubmatch([]byte(v)); s != nil {
			if len(s) > 1 {
				m.SetAddressHeader("To", string(s[1]), "")
			}
		}
	}
	m.SetHeader("Subject", "Mail delivery failed: returning message to sender")
	m.SetHeader("Return-path", "<>")
	m.SetDateHeader("X-Date", time.Now())
	header := e.Data.String()
	headerEnd := strings.Index(header, "\n\n")
	header = header[:headerEnd]
	message := fmt.Sprintf("This message was created automatically by mail delivery software. "+
		"<b>A message that you sent could not be delivered to one or more of its"+
		"recipients<\b>. This is a permanent error.\n The following address(es) failed:<b>\n %s@%s \n<\b>."+
		"Errors: %s.\n -----This is the copy of the message header -----\n %s \n", to.User, to.Host, err.Error(), header)
	m.SetBody("text/html", message)
	m.WriteTo(&re.Data)
	log.Printf("[%s] Generate return email to %s as %s.\n", e.UUID, m.GetHeader("To")[0], re.UUID)
	s.mailQueue <- re
}

type sortMX []*net.MX

func (a sortMX) Len() int           { return len(a) }
func (a sortMX) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a sortMX) Less(i, j int) bool { return a[i].Pref < a[j].Pref }

func (s *MainPolicy) sendMail(e MyEnvelope, to mail.Address, url *url.URL) {
	const (
		success = iota
		retry
		failed
	)
	parseErr := func(err error) (int) {
		if err == nil {
			return success
		}
		log.Printf("Send %s not success: %s.\n", e.UUID, err)
		if v, ok := err.(*textproto.Error); ok {
			if v.Code/100 == 5 {
				return failed
			} else {
				return retry
			}
		}
		return retry
	}
	status := retry
	var err error
	latencyMap := []int{1, 5, 10, 30, 60}
	for i := 0; i < s.RetryCount && status == retry; i++ {
		var addresses = make([]string, 0)
		var auth smtp.Auth = nil
		if url.Scheme == "smtp" {
			addresses = append(addresses, url.Host)
			if url.User.Username() != "" {
				password, _ := url.User.Password()
				host,_,_ := net.SplitHostPort(url.Host)
				auth = smtp.PlainAuth("", url.User.Username(), password, host)
			}
		} else if url.Scheme == "default" {
			host, err := net.LookupMX(to.Host)
			if err != nil {
				err = errors.New("ns lookup failed")
				continue
			}
			sort.Sort(sortMX(host))
			for _, v := range host {
				addresses = append(addresses, v.Host+":smtp")
			}
		}
		f := e.MailFrom.User + "@" + e.MailFrom.Host
		t := append(make([]string, 0), to.User+"@"+to.Host)
		log.Printf("[%s] Send from %s to %s using %s.\n", e.UUID, f, t[0], addresses)
		err = s.smtp(addresses, auth, f, t, []byte(e.Data.String()))
		status = parseErr(err)
		if status == retry {
			time.Sleep(time.Duration(latencyMap[i]) * time.Minute)
		}
	}
	defer e.Delete(s.QueuePath + "/" + e.UUID + ".mail")
	if status == failed {
		if !e.IsDSN {
			s.returnMail(e, to, err)
		}
	}
	if status == success {
		log.Printf("[%s] Send succeed.\n", e.UUID)
	}
}

func (s *MainPolicy) smtp(addresses []string, a smtp.Auth, from string, to []string, msg []byte) error {
	s.borrow()
	defer s.repay()
	var err error
	var c *smtp.Client = nil
	var addr string
	for _, address := range addresses {
		c, err = smtp.Dial(address)
		if err != nil {
			continue
		}
		addr = address
		break
	}
	if err != nil {
		return err
	}
	defer c.Close()
	if err = c.Hello(s.SmtpHostName); err != nil {
		return err
	}
	if ok, _ := c.Extension("STARTTLS"); ok {
		serverName, _, _ := net.SplitHostPort(addr)
		config := &tls.Config{ServerName: serverName, InsecureSkipVerify: true}
		if err = c.StartTLS(config); err != nil {
			return err
		}
	}
	if a != nil {
		if err = c.Auth(a); err != nil {
			return err
		}
	}
	if err = c.Mail(from); err != nil {
		return err
	}
	for _, addr := range to {
		if err = c.Rcpt(addr); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	_, err = w.Write(msg)
	if err != nil {
		return err
	}
	err = w.Close()
	if err != nil {
		return err
	}
	return c.Quit()
}

func (s *MainPolicy) load() {
	files, _ := ioutil.ReadDir(s.QueuePath)
	for _, file := range files {
		if file.IsDir() {
			continue
		} else if path.Ext(file.Name()) == ".mail" {
			e := MyEnvelope{}
			e.Load(s.QueuePath + "/" + file.Name())
			s.mailQueue <- e
		}
	}
}

func (s *MainPolicy) Run() {
	err := s.daemon.Start()
	if err != nil {
		log.Panic(err)
	}
	s.loop()
	s.load()
	<-make(chan bool)
}
