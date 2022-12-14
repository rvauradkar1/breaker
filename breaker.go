package breaker

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// CommandFuncs is implemented by clients, mandatory for clients
// Clients need ensure that they do not panic. If CommandFunc panics,
// DefaultFunc and CleanupFunc are called in order. If DefaultFunc panics, then CleanupFunc is NEVER called
type CommandFuncs interface {
	Name() string // Helps in logging and metrics generation
	CommandFunc() // Function to do the actual work
	DefaultFunc() // Function called by breaker in case of timeout, client implements default behavior
	CleanupFunc() // Function called by breaker in case of timeout. client implements any cleanup actions
}

// Timeout is optionally implemented by clients to override the global circuit breaker timeout
type Timeout interface {
	timeout() time.Duration
}

// Breaker struct for circuit breaker control parameters
type Breaker struct {
	name                string        // For debudding purposes
	timeout             time.Duration // Timeout at breaker level, can be reset by specific consumer
	numConcurrent       int           // Number of concurrent requests
	semaphore           chan bool     // Controls access to execute tasks
	isOk                bool          // Can circuit take more load?
	isShutdown          bool          // Has circuit been shutdown completely?
	status              int           // States for a circuit, look at consts below
	HealthCheckInterval time.Duration // Scanning interval to reset tripped circuit
}

var log *logrus.Logger

// New initializes the circuit breaker
func New(name string, timeout time.Duration, numConcurrent int) *Breaker {
	b := Breaker{}
	b.name = name
	b.timeout = timeout
	b.numConcurrent = numConcurrent
	b.semaphore = make(chan bool, b.numConcurrent)
	b.isOk = true
	b.HealthCheckInterval = 100 // Defaulted to 100 ms, can be overridden
	log = initLog()
	log.Formatter = new(logrus.JSONFormatter)
	go healthcheck(&b) // Start goroutine to start healthcheck
	return &b
}

func initLog() *logrus.Logger {
	log := logrus.New()
	//file, err := os.OpenFile("breaker.log", os.O_RDWR|os.O_CREATE, 666)
	log.Out = os.Stderr
	//fmt.Println(err)
	return log
}

const (
	iShutdown        = 10
	iCircuitStillBad = 20
	iCircuitGood     = 30
)

func healthcheck(b *Breaker) {
	for {
		if b.isShutdown {
			return
		}
		time.Sleep(b.HealthCheckInterval * time.Millisecond)
		if !b.isOk {
			select {
			case b.semaphore <- true:
				<-b.semaphore
				b.closeCircuit()
				fmt.Println("repaired")
				log.WithFields(logrus.Fields{"name": b.name}).Info("circuit repaired, load it normal")
				b.status = iCircuitGood
			default:
				fmt.Println("circuit still bad")
				log.WithFields(logrus.Fields{"name": b.name}).Info("attempt to repair circuit failed")
				b.status = iCircuitStillBad
			}
		}
	}
}

func (b *Breaker) openCircuit() bool {
	b.isOk = false
	b.status = iCircuitStillBad
	return b.isOk
}

func (b *Breaker) closeCircuit() bool {
	b.isOk = true
	b.status = iCircuitGood
	return b.isOk
}

var mutex = &sync.Mutex{}

// Shutdown is called by clients to completely stop circuit breaker from taking any more load
func (b *Breaker) Shutdown() {
	if b.isShutdown {
		return
	}
	mutex.Lock()
	b.isShutdown = true
	mutex.Unlock()
	b.status = iShutdown
}

// Execute is called by clients to initiate task
func (b *Breaker) Execute(commands CommandFuncs) chan Error {
	errorch := make(chan Error, 1)
	if b.isShutdown {
		be := Error{Err: errors.New("circuit has been permanently shutdown. create a new one")}
		errorch <- be
		return errorch
	}
	go func() {
		select {
		case b.semaphore <- true:
			go func() {
				// Have to release token
				defer func() { <-b.semaphore }()
				// Channel for signalling completion of command
				done := make(chan bool, 1)
				go func() {
					defer func() { done <- true }()
					commands.CommandFunc()
				}()
				// Deals with timeout of command
				select {
				case <-time.After(b.commandTimeout(commands)):
					// Call default and cleanup
					commands.DefaultFunc()
					commands.CleanupFunc()
					log.WithFields(logrus.Fields{"name": b.name}).Info("task timed out")
					// Return timeout error
					be := Error{isTimeout: true, Err: errors.New("task timed out")}
					errorch <- be
				case <-done:
					errorch <- Error{isSuccess: true, Err: nil}
				}
			}()
		default:
			commands.DefaultFunc()
			commands.CleanupFunc()
			b.openCircuit()
			errorch <- Error{isSuccess: false, Err: errors.New("reached threshold, cannot run your command")}
		}
	}()
	return errorch
}

func (b *Breaker) commandTimeout(c CommandFuncs) time.Duration {
	if t, ok := c.(Timeout); ok {
		return t.timeout()
	}
	return b.timeout
}

// Error can be unwrappd by clients to determine exact nature of failure
type Error struct {
	Err        error
	isTimeout  bool
	isShutdown bool
	isSuccess  bool
}

func (b Error) Unwrap() error  { return b.Err }
func (b Error) Error() string  { return b.Err.Error() }
func (b Error) Timeout() bool  { return b.isTimeout }
func (b Error) Success() bool  { return b.isSuccess }
func (b Error) Shutdown() bool { return b.isShutdown }
