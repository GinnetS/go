package main

import (
	"bufio"
	"encoding/csv"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

type PingResult struct {
	Host  string
	Alive bool
	RTT   time.Duration
	Err   string
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
func getenvStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func readHosts(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	var hosts []string
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		hosts = append(hosts, line)
	}
	return hosts, s.Err()
}

var seq uint32

// un intento ICMP usando un socket compartido por worker
func pingOnceWithConn(conn *icmp.PacketConn, dst *net.IPAddr, timeout time.Duration, id, seq int) (time.Duration, error) {
	_ = conn.SetDeadline(time.Now().Add(timeout))

	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{ID: id & 0xffff, Seq: seq & 0xffff, Data: []byte("pingpool")},
	}
	b, err := msg.Marshal(nil)
	if err != nil {
		return 0, fmt.Errorf("marshal: %w", err)
	}

	start := time.Now()
	if _, err := conn.WriteTo(b, dst); err != nil {
		return 0, fmt.Errorf("write: %w", err)
	}

	buf := make([]byte, 1500)
	for {
		n, peer, err := conn.ReadFrom(buf)
		if err != nil {
			return 0, fmt.Errorf("read: %w", err)
		}
		rtt := time.Since(start)

		// valida origen de la respuesta
		peerIP := ""
		switch p := peer.(type) {
		case *net.IPAddr:
			peerIP = p.IP.String()
		default:
			peerIP = peer.String()
		}
		if !net.IP.Equal(dst.IP, net.ParseIP(strings.Split(peerIP, "%")[0])) {
			continue // otra respuesta, seguimos hasta deadline
		}

		rec, err := icmp.ParseMessage(1, buf[:n]) // 1 = ICMPv4
		if err != nil {
			return 0, fmt.Errorf("parse: %w", err)
		}
		if rec.Type == ipv4.ICMPTypeEchoReply {
			if body, ok := rec.Body.(*icmp.Echo); ok && body.ID == (id&0xffff) {
				return rtt, nil
			}
		}
	}
}

func worker(id int, jobs <-chan string, results chan<- PingResult, count int, timeout time.Duration) {
	// un socket por worker (estable y eficiente en FD)
	conn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		for host := range jobs {
			results <- PingResult{Host: host, Alive: false, Err: fmt.Sprintf("icmp listen: %v", err)}
		}
		return
	}
	defer conn.Close()

	for host := range jobs {
		ipAddr, rerr := net.ResolveIPAddr("ip4", host)
		if rerr != nil {
			results <- PingResult{Host: host, Alive: false, Err: fmt.Sprintf("resolve: %v", rerr)}
			continue
		}
		var lastErr error
		var rtt time.Duration
		ok := false
		for i := 0; i < count; i++ {
			seqNum := int(atomic.AddUint32(&seq, 1))
			rtt, lastErr = pingOnceWithConn(conn, ipAddr, timeout, id, seqNum)
			if lastErr == nil {
				ok = true
				break
			}
		}
		results <- PingResult{
			Host:  host,
			Alive: ok,
			RTT:   rtt,
			Err:   func() string { if lastErr != nil { return lastErr.Error() }; return "" }(),
		}
	}
}

func main() {
	// valores por env (override con flags)
	defWorkers := getenvInt("WORKERS", 600)      // p. ej. 600 workers para 3000 hosts
	defTimeout := getenvInt("TIMEOUT_MS", 1000)  // 1s
	defCount := getenvInt("COUNT", 1)            // 1 intento
	hostsFile := getenvStr("HOSTS_FILE", "/data/hosts.txt")
	outputFile := getenvStr("OUTPUT_FILE", "/out/results.csv")

	workers := flag.Int("workers", defWorkers, "cantidad de workers")
	timeoutMs := flag.Int("timeout", defTimeout, "timeout por ping (ms)")
	count := flag.Int("count", defCount, "intentos de ping por host")
	flag.Parse()

	hosts, err := readHosts(hostsFile)
	if err != nil {
		log.Fatalf("error leyendo hosts: %v", err)
	}
	if len(hosts) == 0 {
		log.Fatalf("%s no tiene hosts", hostsFile)
	}

	log.Printf("Ping a %d hosts con %d workers (timeout=%dms, count=%d)\n", len(hosts), *workers, *timeoutMs, *count)

	jobs := make(chan string)
	results := make(chan PingResult)

	var wg sync.WaitGroup
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		id := i + 1
		go func() {
			defer wg.Done()
			worker(id, jobs, results, *count, time.Duration(*timeoutMs)*time.Millisecond)
		}()
	}

	// recolector
	var collectorWg sync.WaitGroup
	collectorWg.Add(1)

	var aliveCount int64
	go func(total int) {
		defer collectorWg.Done()
		if err := os.MkdirAll("/out", 0o755); err != nil {
			log.Fatalf("no se pudo crear /out: %v", err)
		}
		f, err := os.Create(outputFile)
		if err != nil {
			log.Fatalf("no se pudo crear %s: %v", outputFile, err)
		}
		defer f.Close()
		w := csv.NewWriter(f)
		defer w.Flush()
		_ = w.Write([]string{"host", "alive", "rtt_ms", "error"})
		for i := 0; i < total; i++ {
			res := <-results
			if res.Alive {
				atomic.AddInt64(&aliveCount, 1)
			}
			rttMs := ""
			if res.RTT > 0 {
				rttMs = fmt.Sprintf("%.2f", float64(res.RTT.Microseconds())/1000.0)
			}
			_ = w.Write([]string{res.Host, strconv.FormatBool(res.Alive), rttMs, res.Err})
			log.Printf("%s -> alive=%v rtt=%s err=%s", res.Host, res.Alive, rttMs, res.Err)
		}
	}(len(hosts))

	// enqueue
	go func() {
		for _, h := range hosts {
			jobs <- h
		}
		close(jobs)
	}()

	// cerrar results cuando terminen los workers
	go func() {
		wg.Wait()
		close(results)
	}()

	collectorWg.Wait()
	log.Printf("Completado. %d/%d hosts responden. Resultados en %s\n", aliveCount, len(hosts), outputFile)
}
