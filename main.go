package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	expirablecache "github.com/go-pkgz/expirable-cache/v2"
	"github.com/idsulik/go-collections/set"
	"github.com/miekg/dns"
)

type dnsHandler struct {
	//allDnsServers    []string // custom dns servers are in the first elements of []
	dnsServers  map[string]*set.Set[string]
	userDomains *set.Set[string]
	dnsCache    expirablecache.Cache[string, dns.Msg]
}

func DnsHandler(servers string, domains string) *dnsHandler {
	h := new(dnsHandler)
	h.dnsCache = expirablecache.NewCache[string, dns.Msg]().WithMaxKeys(1000).WithTTL(time.Minute * 10)
	h.dnsServers = map[string]*set.Set[string]{"custom": set.New[string](), "system": set.New[string]()}
	h.userDomains = set.New[string]()
	h.dnsServers["custom"].AddAll(strings.Split(servers, ",")...)
	h.userDomains.AddAll(strings.Split(domains, ",")...)
	log.Printf("User input: servers: %s, domains: %s", h.dnsServers["custom"].Elements(), h.userDomains.Elements())
	return h
}

func (h *dnsHandler) observeSystemDNSServers() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		panic(err)
	}
	defer watcher.Close()
	err = watcher.Add("/var/run")
	if err != nil {
		panic(err)
	}
	for {
		select {
		case event := <-watcher.Events:
			if event.Op&fsnotify.Create == fsnotify.Create ||
				event.Op&fsnotify.Write == fsnotify.Write {
				if strings.Contains(event.Name, "resolv.conf") {
					h.updateAllDNSServers()
				}
			}
		case err := <-watcher.Errors:
			log.Println("fsnotify error:", err)
		}
	}
}

func (h *dnsHandler) updateAllDNSServers() {
	file, err := os.Open("/etc/resolv.conf")
	if err != nil {
		log.Println("Error opening /etc/resolv.conf")
		return
	}
	defer file.Close()

	h.dnsCache.Purge() //we most probably changed network
	h.dnsServers["system"].Clear()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Look for lines that start with "nameserver"
		if strings.HasPrefix(line, "nameserver") {
			parts := strings.Fields(line)
			if len(parts) == 2 {
				server := parts[1]
				if strings.Contains(server, "127.0.0.1") {
					server = "1.1.1.1"
					log.Println("Using 1.1.1.1 insead of system set DNS 127.0.0.1 to avoid loops")
				}
				if strings.Contains(server, ":") { //ipv6
					server = "[" + server + "]"
				}
				if !h.dnsServers["custom"].Has(server) {
					h.dnsServers["system"].Add(server)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		log.Println("Scanner error")
	}
	log.Printf("All DNS Servers: custom: %s, system: %s", h.dnsServers["custom"].Elements(), h.dnsServers["system"].Elements())
}

func (h *dnsHandler) handleDNSRequest(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	question := r.Question[0]
	cacheKey := fmt.Sprintf("%s", question.Name)
	var timeout time.Duration = 0 //0 honors global setting
	var forwardedResponse *dns.Msg
	var err error

	// Check the cache for a previous response
	response, cacheExists := h.dnsCache.Get(cacheKey)
	if cacheExists {
		// Serve the response from the cache
		m.Answer = append(m.Answer, response.Answer...)
		log.Printf("DNS query for %s served by CACHE", question.Name)
	} else {
		queryServers := []string{}
		// Check query if in custom domains
		for _, domain := range h.userDomains.Elements() {
			if strings.HasSuffix(strings.TrimSuffix(question.Name, "."), domain) {
				queryServers = append(queryServers, h.dnsServers["custom"].Elements()...)
				break
			}
		}
		queryServers = append(queryServers, h.dnsServers["system"].Elements()...)

		// Forward DNS query to defined DNS servers
		if len(queryServers) == 0 {
			err = errors.New("Empty server list")
		} else {
			for _, server := range queryServers {
				forwardedResponse, err = dns.Exchange(r, server+":53")
				if err == nil {
					log.Printf("DNS query for %s served by %s", question.Name, server)
					break
				} else {
					log.Printf("DNS query for %s failed using server %s, error was: %s", question.Name, server, err)

				}
			}
		}

		if err != nil {
			log.Printf("Error forwarding DNS request: %s", err)
			m.SetRcode(r, dns.RcodeServerFailure)
			w.WriteMsg(m)
			return
		}

		// Check the query type (A record or IPv4 address)
		if question.Qtype == dns.TypeA {
			m.Answer = append(m.Answer, forwardedResponse.Answer...)
			// Cache the response
			if len(forwardedResponse.Answer) != 0 {
				h.dnsCache.Set(cacheKey, *forwardedResponse, timeout)
			}
		} else {
			// Handle other query types or respond with an error
			m.SetRcode(r, dns.RcodeNameError)
		}
	}
	w.WriteMsg(m)
}

func (h *dnsHandler) deleteResolvers() {
	for _, domain := range h.userDomains.Elements() {
		filename := "/etc/resolver/" + domain
		err := os.Remove(filename)
		if err != nil {
			log.Printf("Error removing file %s: %v", filename, err)
			continue
		}
		log.Println("Deleted generated resolver server file ", filename)
	}
}

func (h *dnsHandler) createResolvers() {
	dir := "/etc/resolver/"
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		err = os.Mkdir(dir, 0755)
		if err != nil {
			log.Printf("ERROR: %v. You either need to run with sudo or manually create the folder /etc/resolver and chown it to your user\n", err)
			os.Exit(3)
		}
	}
	// For each domain, create a file and write the servers to it
	for _, domain := range h.userDomains.Elements() {
		filename := dir + domain

		file, err := os.Create(filename)
		if err != nil {
			log.Printf("ERROR: %v. You either need to run with sudo or chown /etc/resolver to your user\n", err)
			os.Exit(3)
		}
		defer file.Close()
		_, err = file.WriteString("nameserver 127.0.0.1\n")
		log.Println("Successfully created resolver server file ", filename)
	}
}

func (handler *dnsHandler) run() {
	handler.createResolvers()
	handler.updateAllDNSServers()
	go handler.observeSystemDNSServers()
	defer handler.deleteResolvers()
	log.Println("Starting DNS Local UDP server...")
	dns.HandleFunc(".", handler.handleDNSRequest)
	server := &dns.Server{
		Addr:      ":53",
		Net:       "udp",
		UDPSize:   65535,
		ReusePort: true,
	}
	go func() {
		err := server.ListenAndServe()
		if err != nil {
			log.Panic(err)
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	s := <-sig
	log.Printf("Signal (%v) received, stopping\n", s)
}

func removeDuplicates(s []string) []string {
	bucket := make(map[string]bool)
	var result []string
	for _, str := range s {
		if _, ok := bucket[str]; !ok {
			bucket[str] = true
			result = append(result, str)
		}
	}
	return result
}

func main() {
	if runtime.GOOS != "darwin" {
		log.Fatal("dnsforwarder is designed for macos")
	}
	var dnsServers string
	var dnsDomains string
	flag.StringVar(&dnsServers, "servers", "", "Comma separated list of custom DNS Servers")
	flag.StringVar(&dnsDomains, "domains", "", "Comma separated list of custom DNS Domains")
	flag.Parse()

	if len(dnsServers) == 0 || len(dnsDomains) == 0 {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n Example: \n %v --servers 1.0.0.1 --domains comanya.com,company-resources.com\n\n", os.Args[0])
		os.Exit(1)
	}

	DnsHandler(dnsServers, dnsDomains).run()
}
