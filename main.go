package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	expirablecache "github.com/go-pkgz/expirable-cache/v2"
	"github.com/miekg/dns"
)

type dnsHandler struct {
	allDnsServers    []string // custom dns servers are in the first elements of []
	customDnsServers []string
	customDnsDomains []string
	dnsCache         expirablecache.Cache[string, dns.Msg]
}

func (h *dnsHandler) init(customServers string, customDomains string) {
	h.dnsCache = expirablecache.NewCache[string, dns.Msg]().WithMaxKeys(1000).WithTTL(time.Minute * 5)
	h.customDnsServers = removeDuplicates(strings.Split(customServers, ","))
	h.customDnsDomains = removeDuplicates(strings.Split(customDomains, ","))
	fmt.Printf("User input: servers '%s', domains '%s' \n", strings.Join(h.customDnsServers, " "), strings.Join(h.customDnsDomains, " "))
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
			fmt.Println("fsnotify error:", err)
		}
	}
}

func (h *dnsHandler) updateAllDNSServers() {
	systemDnsServers := []string{}
	file, err := os.Open("/etc/resolv.conf")
	if err != nil {
		fmt.Println("Error opening /etc/resolv.conf")
		return
	}
	defer file.Close()
	localhostFound := false
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Look for lines that start with "nameserver"
		if strings.HasPrefix(line, "nameserver") {
			parts := strings.Fields(line)
			if len(parts) == 2 {
				server := parts[1]
				if strings.Contains(server, "127.0.0.1") {
					localhostFound = true
				} else {
					if strings.Contains(server, ":") { //ipv6
						server = "[" + server + "]"
					}
					systemDnsServers = append(systemDnsServers, server)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Println("Scanner error")
	}
	if localhostFound && len(systemDnsServers) == 0 {
		h.allDnsServers = append(systemDnsServers, "1.1.1.1")
		fmt.Println("Using 1.1.1.1 insead of system set DNS 127.0.0.1 to avoid loops")
	}

	servers := append(h.customDnsServers, systemDnsServers...)
	h.allDnsServers = removeDuplicates(servers)
	fmt.Println("Final DNS list including system-set DNS servers ", h.allDnsServers)
}

func (h *dnsHandler) handleDNSRequest(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	question := r.Question[0]
	cacheKey := fmt.Sprintf("%s", question.Name)
	var timeout time.Duration = 0

	// Check the cache for a previous response
	response, cacheExists := h.dnsCache.Get(cacheKey)
	var forwardedResponse *dns.Msg
	var err error

	if cacheExists {
		// Serve the response from the cache
		m.Answer = append(m.Answer, response.Answer...)
		fmt.Printf("DNS query for %s served by CACHE \n", question.Name)
	} else {
		// Check query if in custom domains
		customForward := false
		for _, domain := range h.customDnsDomains {
			if strings.Contains(question.Name, domain) {
				customForward = true
			}
		}

		// Forward DNS query to defined DNS servers
		for i, server := range h.allDnsServers {
			if !customForward && i <= len(h.customDnsServers) {
				//the domain is not in custom domains;
				//skip query on the custom servers (system dns set to 127.0.0.1 scenario)
				continue
			}
			forwardedResponse, err = dns.Exchange(r, server+":53")
			if err == nil {
				fmt.Printf("DNS query for %s served by %s \n", question.Name, server)
				break
			}
		}

		if err != nil {
			fmt.Println("Error forwarding DNS request; all servers tried; error from last server was ", err)
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
	for _, domain := range h.customDnsDomains {
		filename := "/etc/resolver/" + domain
		err := os.Remove(filename)
		if err != nil {
			fmt.Printf("Error removing file %s: %v\n", filename, err)
			continue
		}
		fmt.Println("Deleted generated resolver server file ", filename)
	}
}

func (h *dnsHandler) createResolvers() {
	dir := "/etc/resolver/"
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		os.Mkdir(dir, 0755)
	}
	// For each domain, create a file and write the servers to it
	for _, domain := range h.customDnsDomains {
		filename := dir + domain

		file, err := os.Create(filename)
		if err != nil {
			fmt.Printf("Error creating file for domain %s: %v\n", domain, err)
			continue
		}
		defer file.Close()
		_, err = file.WriteString("nameserver 127.0.0.1\n")
		fmt.Println("Successfully created resolver server file ", filename)
	}
}

func (handler dnsHandler) run(servers string, domains string) {
	handler.init(servers, domains)
	handler.createResolvers()
	handler.updateAllDNSServers()
	go handler.observeSystemDNSServers()
	fmt.Println("Starting UDP server...")
	//handler := dns.DefaultServeMux
	dns.HandleFunc(".", handler.handleDNSRequest)
	server := &dns.Server{
		Addr: ":53",
		Net:  "udp",
		//Handler:   handler,
		UDPSize:   65535,
		ReusePort: true,
	}
	go func() {
		err := server.ListenAndServe()
		if err != nil {
			panic(err)
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	s := <-sig
	fmt.Printf("Signal (%v) received, stopping\n", s)
	handler.deleteResolvers()
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
		fmt.Println("dnsforwarder is designed for macos")
		os.Exit(2)
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

	handler := new(dnsHandler)
	handler.run(dnsServers, dnsDomains)
}
