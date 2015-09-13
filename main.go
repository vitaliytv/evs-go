package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	valid "github.com/asaskevich/govalidator"
	"github.com/julienschmidt/httprouter"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type configuration struct {
	IP                        string `json:"server.ip"`
	Port                      int    `json:"server.port"`
	Password                  string `json:"server.password"`
	WorkersCount              int    `json:"work.workers"`
	WorkBufferSize            int    `json:"work.buffersize"`
	CheckEmailFrom            string `json:"email.from"`
	EmailsCacheEnabled        bool   `json:"emails.cache.enabled"`
	EmailsCacheGCFrequency    int    `json:"emails.cache.gcfrequency"`
	EmailsCacheMaxSize        int    `json:"emails.cache.maxsize"`
	DomainsMXCacheEnabled     bool   `json:"domains.mxcache.enabled"`
	DomainsMXCacheGCFrequency int    `json:"domains.mxcache.gcfrequency"`
	DomainsMXCacheMaxSize     int    `json:"domains.mxcache.maxsize"`
	DomainsMXQueryTimeout     int    `json:"domains.mxquery.timeout"`
	Verbose                   bool   `json:"verbose"`
	Vduration                 bool   `json:"vduration"`
}

func newConfiguration() *configuration {
	return &configuration{
		IP:                        "127.0.0.1",
		Port:                      8000,
		Password:                  "",
		WorkersCount:              32,
		WorkBufferSize:            64,
		CheckEmailFrom:            "noreply@domain.com",
		EmailsCacheEnabled:        true,
		EmailsCacheGCFrequency:    86400,
		EmailsCacheMaxSize:        10000,
		DomainsMXCacheEnabled:     true,
		DomainsMXCacheGCFrequency: 2592000,
		DomainsMXCacheMaxSize:     1000,
		DomainsMXQueryTimeout:     5,
		Verbose:                   false,
		Vduration:                 false,
	}
}

func (c *configuration) loadFromJSONFile(configFile string) {
	currentPath, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		log.Fatal(err)
	}
	configFilePath := currentPath + string(os.PathSeparator) + configFile

	_, err = os.Stat(configFilePath)
	if err != nil {
		return
	}

	b, err := ioutil.ReadFile(configFilePath)
	if err != nil {
		log.Fatalf("Configuration file read error: %s", err)
	}

	err = json.Unmarshal(b, c)
	if err != nil {
		log.Fatalf("Configuration file marshal error: %s", err)
	}
}

type domainsMXCacheDataItem struct {
	key string
	val []*net.MX
}

type domainsMXCacheDataItems []*domainsMXCacheDataItem

type domainsMXCache struct {
	sync.Mutex
	maxSize     int
	gcFrequency time.Duration
	data        domainsMXCacheDataItems
}

func (d *domainsMXCache) add(k string, v []*net.MX) {
	d.Lock()
	defer d.Unlock()
	if len(d.data) >= d.maxSize {
		d.data = d.data[1:]
	}
	d.data = append(d.data, &domainsMXCacheDataItem{k, v})
}

func (d *domainsMXCache) get(k string) ([]*net.MX, bool) {
	d.Lock()
	defer d.Unlock()
	for _, s := range d.data {
		if s.key == k {
			return s.val, true
		}
	}
	return nil, false
}

func (d *domainsMXCache) gcHandler() {
	ticker := time.NewTicker(d.gcFrequency)
	for _ = range ticker.C {
		d.Lock()
		d.data = d.data[:0]
		d.Unlock()
	}
}

func newDomainsMXCache() *domainsMXCache {
	d := &domainsMXCache{
		gcFrequency: time.Second * time.Duration(config.DomainsMXCacheGCFrequency),
		maxSize:     config.DomainsMXCacheMaxSize,
	}
	if config.DomainsMXCacheGCFrequency > 0 {
		go d.gcHandler()
	}
	return d
}

type emailsCacheDataItem struct {
	key, val string
}

type emailsCacheDataItems []*emailsCacheDataItem

type emailsCache struct {
	sync.RWMutex
	maxSize     int
	gcFrequency time.Duration
	data        emailsCacheDataItems
}

func (e *emailsCache) add(k string, v string) {
	e.Lock()
	defer e.Unlock()
	if len(e.data) >= e.maxSize {
		e.data = e.data[1:]
	}
	e.data = append(e.data, &emailsCacheDataItem{k, v})
}

func (e *emailsCache) get(k string) (string, bool) {
	e.Lock()
	defer e.Unlock()
	for _, s := range e.data {
		if s.key == k {
			return s.val, true
		}
	}
	return "", false
}

func (e *emailsCache) gcHandler() {
	ticker := time.NewTicker(e.gcFrequency)
	for _ = range ticker.C {
		e.Lock()
		e.data = e.data[:0]
		e.Unlock()
	}
}

func newEmailsCache() *emailsCache {
	e := &emailsCache{
		gcFrequency: time.Second * time.Duration(config.EmailsCacheGCFrequency),
		maxSize:     config.EmailsCacheMaxSize,
	}
	if config.EmailsCacheGCFrequency > 0 {
		go e.gcHandler()
	}
	return e
}

type httpJSONResponse struct {
	Status  string            `json:"status"`
	Message string            `json:"message"`
	Emails  map[string]string `json:"emails"`
}

type incomingEmails []string
type outgoingEmails struct {
	sync.Mutex
	Emails map[string]string `json:"emails"`
}

func newOutgoingEmails(emLen int) *outgoingEmails {
	return &outgoingEmails{
		Emails: make(map[string]string, emLen),
	}
}

func (o *outgoingEmails) Add(k, v string) {
	o.Lock()
	defer o.Unlock()
	o.Emails[k] = v
}

var (
	config   *configuration
	dMXCache *domainsMXCache
	eCache   *emailsCache
)

func veResVal(email, message string) string {
	if config.EmailsCacheEnabled {
		eCache.add(email, message)
	}
	return message
}

func validateEmail(email string) string {
	if config.EmailsCacheEnabled {
		if r, ok := eCache.get(email); ok {
			return r
		}
	}

	if len(email) > 255 || !valid.IsEmail(strings.ToLower(email)) {
		return veResVal(email, "invalid email address")
	}
	domainName := strings.Split(email, "@")[1]

	var mxRecords []*net.MX
	fetchedFromCache := false
	if config.DomainsMXCacheEnabled {
		if tmxRecords, ok := dMXCache.get(domainName); ok {
			mxRecords = tmxRecords
			tmxRecords = nil
			fetchedFromCache = true
		}
	}

	if !fetchedFromCache && len(mxRecords) == 0 {
		tmxRecords, err := net.LookupMX(domainName)
		if err != nil {
			return err.Error()
		}
		mxRecords = tmxRecords
		tmxRecords = nil
	}

	if !fetchedFromCache && config.DomainsMXCacheEnabled {
		dMXCache.add(domainName, mxRecords)
	}

	if len(mxRecords) == 0 {
		return "no mx record found"
	}

	for _, n := range mxRecords {
		addr := fmt.Sprintf("%s:%d", strings.Trim(n.Host, "."), 25)
		conn, err := net.DialTimeout("tcp", addr, time.Second*time.Duration(config.DomainsMXQueryTimeout))
		if err != nil {
			continue
		}

		host, _, _ := net.SplitHostPort(addr)
		c, err := smtp.NewClient(conn, host)
		if err != nil {
			continue
		}

		defer c.Quit()
		defer c.Close()

		if err = c.Hello(domainName); err != nil {
			return veResVal(email, err.Error())
		}

		if ok, _ := c.Extension("STARTTLS"); ok {
			tlsConfig := &tls.Config{ServerName: domainName, InsecureSkipVerify: true}
			if err = c.StartTLS(tlsConfig); err != nil {
				return veResVal(email, err.Error())
			}
		}

		if err = c.Mail(config.CheckEmailFrom); err != nil {
			return veResVal(email, err.Error())
		}

		if err = c.Rcpt(email); err != nil {
			return veResVal(email, err.Error())
		}

		return veResVal(email, "OK")
	}

	return veResVal(email, "OK")
}

func worker(work <-chan string, o *outgoingEmails, wg *sync.WaitGroup, wnum int) {
	defer wg.Done()
	for email := range work {
		tStart := time.Now()
		res := validateEmail(email)
		tElapsed := time.Since(tStart)
		if config.Vduration {
			res += fmt.Sprintf(" [took %s]", tElapsed)
		}
		o.Add(email, res)
		if config.Verbose {
			fmt.Println("Worker #", wnum, "done", email, "in", tElapsed)
		}
	}
}

func setupHTTP(fn httprouter.Handle) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		w.Header().Set("Content-Type", "application/json")
		fn(w, r, ps)
	}
}

func sendHTTPJSONResponse(w http.ResponseWriter, status, message string, emails map[string]string) {
	js, err := json.Marshal(&httpJSONResponse{status, message, emails})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fmt.Fprint(w, string(js))
	return
}

func httpHandler(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	start := time.Now()

	if len(config.Password) > 0 && r.Header.Get("Authorization") != config.Password {
		sendHTTPJSONResponse(w, "error", "Invalid password", nil)
		return
	}

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		sendHTTPJSONResponse(w, "error", "Invalid payload", nil)
		return
	}

	var iem incomingEmails
	err = json.Unmarshal(body, &iem)
	if err != nil {
		sendHTTPJSONResponse(w, "error", "Invalid payload", nil)
		return
	}

	// remove duplicates.
	var emails []string
	tmp := make(map[string]bool)
	for _, e := range iem {
		if _, ok := tmp[e]; ok {
			continue
		}
		tmp[e] = true
		emails = append(emails, e)
	}

	iem = nil
	tmp = nil

	wbSize := config.WorkBufferSize
	wCount := config.WorkersCount
	eCount := len(emails)

	if eCount < wCount {
		wCount = eCount
		wbSize = 1
	}

	wg := &sync.WaitGroup{}
	work := make(chan string, wbSize)
	o := newOutgoingEmails(eCount)
	for i := 0; i < wCount; i++ {
		wg.Add(1)
		go worker(work, o, wg, i)
	}

	for _, e := range emails {
		work <- e
	}

	close(work)
	wg.Wait()

	e := time.Since(start)
	m := fmt.Sprintf("Request completed, verified %d emails in %s", eCount, e)
	sendHTTPJSONResponse(w, "success", m, o.Emails)
}

func main() {

	defaultConfig := newConfiguration()
	defaultConfig.loadFromJSONFile("config.json")

	ip := flag.String("server.ip", defaultConfig.IP, "server ip address, empty to bind all interfaces")
	port := flag.Int("server.port", defaultConfig.Port, "server port")
	password := flag.String("server.password", defaultConfig.Password, "the password to allow access to the server via http requests")
	workersCount := flag.Int("work.workers", defaultConfig.WorkersCount, "the number of workers that will process emails at same time")
	workBufferSize := flag.Int("work.buffersize", defaultConfig.WorkBufferSize, "the buffer size for all workers")
	checkEmailFrom := flag.String("email.from", defaultConfig.CheckEmailFrom, "the email address to be used as the MAIL FROM command")
	EmailsCacheEnabled := flag.Bool("emails.cache.enabled", defaultConfig.EmailsCacheEnabled, "whether email cache is enabled")
	EmailsCacheGCFrequency := flag.Int("emails.cache.gcfrequency", defaultConfig.EmailsCacheGCFrequency, "garbage collector frequency for cached emails")
	EmailsCacheMaxSize := flag.Int("emails.cache.maxsize", defaultConfig.EmailsCacheMaxSize, "max items to keep in the cache at any give time")
	domainsMXCacheEnabled := flag.Bool("domains.mxcache.enabled", defaultConfig.DomainsMXCacheEnabled, "whether email cache is enabled for domains mx records")
	domainsMXCacheGCFrequency := flag.Int("domains.mxcache.gcfrequency", defaultConfig.DomainsMXCacheGCFrequency, "garbage collector frequency for cached mx records")
	domainsMXCacheMaxSize := flag.Int("domains.mxcache.maxsize", defaultConfig.DomainsMXCacheMaxSize, "max items to keep in the cache at any give time")
	domainsMXQueryTimeout := flag.Int("domains.mxquery.timeout", defaultConfig.DomainsMXQueryTimeout, "timeout in seconds for MX queries")
	verbose := flag.Bool("verbose", defaultConfig.Verbose, "whether to enable verbose mode")
	vduration := flag.Bool("vduration", defaultConfig.Vduration, "whether to include validation duration for each email address")

	flag.Parse()
	defaultConfig = nil

	config = &configuration{
		IP:                        *ip,
		Port:                      *port,
		Password:                  *password,
		WorkersCount:              *workersCount,
		WorkBufferSize:            *workBufferSize,
		CheckEmailFrom:            *checkEmailFrom,
		EmailsCacheEnabled:        *EmailsCacheEnabled,
		EmailsCacheGCFrequency:    *EmailsCacheGCFrequency,
		EmailsCacheMaxSize:        *EmailsCacheMaxSize,
		DomainsMXCacheEnabled:     *domainsMXCacheEnabled,
		DomainsMXCacheGCFrequency: *domainsMXCacheGCFrequency,
		DomainsMXCacheMaxSize:     *domainsMXCacheMaxSize,
		DomainsMXQueryTimeout:     *domainsMXQueryTimeout,
		Verbose:                   *verbose,
		Vduration:                 *vduration,
	}

	if config.DomainsMXCacheEnabled {
		dMXCache = newDomainsMXCache()
	}

	if config.EmailsCacheEnabled {
		eCache = newEmailsCache()
	}

	address := fmt.Sprintf("%s:%d", config.IP, config.Port)
	router := httprouter.New()
	router.POST("/", setupHTTP(httpHandler))
	log.Fatal(http.ListenAndServe(address, router))
}