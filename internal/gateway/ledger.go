package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Transaction represents a single metered API request through the gateway.
type Transaction struct {
	Timestamp   time.Time `json:"timestamp"`
	Model       string    `json:"model"`
	TokensIn    int       `json:"tokens_in"`
	TokensOut   int       `json:"tokens_out"`
	ACETCost    int       `json:"acet_cost"`    // total ACET charged
	ConsumerKey string    `json:"consumer_key"` // API key prefix or identifier
	Latency     float64   `json:"latency_ms"`
	Path        string    `json:"path"`
}

// Stats summarises gateway activity.
type Stats struct {
	TotalEarnings  int     `json:"total_earnings"`  // lifetime ACET earned (operator share)
	TodayEarnings  int     `json:"today_earnings"`  // ACET earned today
	TotalRequests  int     `json:"total_requests"`  // lifetime request count
	TodayRequests  int     `json:"today_requests"`  // requests today
	AvgLatency     float64 `json:"avg_latency_ms"`  // average latency in ms
}

// Ledger records gateway transactions to an append-only JSONL file.
// It is safe for concurrent use.
type Ledger struct {
	mu      sync.Mutex
	baseDir string
}

// NewLedger creates a ledger that writes to the given base directory.
// The transaction file is created at <baseDir>/gateway/transactions.jsonl.
func NewLedger(baseDir string) *Ledger {
	return &Ledger{baseDir: baseDir}
}

func (l *Ledger) filePath() string {
	return filepath.Join(l.baseDir, "gateway", "transactions.jsonl")
}

// Record appends a transaction to the log file.
func (l *Ledger) Record(tx Transaction) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	dir := filepath.Dir(l.filePath())
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create ledger dir: %w", err)
	}

	f, err := os.OpenFile(l.filePath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open ledger file: %w", err)
	}
	defer f.Close()

	data, err := json.Marshal(tx)
	if err != nil {
		return fmt.Errorf("marshal transaction: %w", err)
	}
	data = append(data, '\n')
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write transaction: %w", err)
	}
	return nil
}

// Recent returns the last n transactions, most recent first.
func (l *Ledger) Recent(n int) ([]Transaction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	all, err := l.readAll()
	if err != nil {
		return nil, err
	}

	// Reverse to get most-recent-first
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}

	if n > len(all) {
		n = len(all)
	}
	return all[:n], nil
}

// StatsFromDisk computes stats by reading the full transaction log.
// This is designed for cross-process reads (e.g., the TUI reading
// the gateway's file).
func (l *Ledger) StatsFromDisk() (Stats, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	all, err := l.readAll()
	if err != nil {
		return Stats{}, err
	}
	return computeStats(all), nil
}

func computeStats(txns []Transaction) Stats {
	if len(txns) == 0 {
		return Stats{}
	}

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	var s Stats
	var totalLatency float64

	for _, tx := range txns {
		// Operator share: ceil(80% of cost)
		operatorShare := int(math.Ceil(float64(tx.ACETCost) * 0.80))
		s.TotalEarnings += operatorShare
		s.TotalRequests++
		totalLatency += tx.Latency

		if !tx.Timestamp.Before(todayStart) {
			s.TodayEarnings += operatorShare
			s.TodayRequests++
		}
	}

	s.AvgLatency = totalLatency / float64(len(txns))
	return s
}

// readAll reads all transactions from the file. Caller must hold l.mu.
func (l *Ledger) readAll() ([]Transaction, error) {
	f, err := os.Open(l.filePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	defer f.Close()

	var txns []Transaction
	scanner := bufio.NewScanner(f)
	// Support long lines (up to 1MB)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var tx Transaction
		if err := json.Unmarshal(line, &tx); err != nil {
			continue // skip malformed lines
		}
		txns = append(txns, tx)
	}
	if err := scanner.Err(); err != nil {
		return txns, fmt.Errorf("scan ledger: %w", err)
	}
	return txns, nil
}
