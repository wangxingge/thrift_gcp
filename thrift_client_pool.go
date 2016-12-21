package thrift_clientpool

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"log"
)

var (
	DefaultKeepAliveInterval time.Duration = time.Second * 3
	DefaultCreateNewInterval time.Duration = time.Second * 1
	DefaultDialRetryCount                  = 3
	DefaultRetryInterval     time.Duration = time.Second * 10
)

type ThriftClientPool struct {
	Name              string
	Dial              func(tag string) (connection interface{}, err error)
	Close             func(tag string, connection interface{}) (err error)
	KeepAlive         func(tag string, connection interface{}) (err error)
	MaxPoolSize       int
	DialRetryCount    int
	KeepAliveInterval time.Duration
	DialRetryInterval time.Duration
	CreateNewInterval time.Duration
	workConnCount     int32
	alivePool         chan interface{}
	swapPool          chan interface{}
	retryPool         chan int
	sync              sync.Mutex
	isStopped         bool
}

func NewThriftClientPool(name string, dialFn func(tag string) (connection interface{}, err error), closeFn func(tag string, connection interface{}) (err error), keepAliveFn func(tag string, connection interface{}) (err error), poolSize, initialPoolSize int) (*ThriftClientPool, error) {

	if dialFn == nil || closeFn == nil || keepAliveFn == nil {
		return nil, errors.New("function not specified.")
	}

	if initialPoolSize < 0 {
		return nil, errors.New("pool size less than 0.")
	}

	if poolSize < 1 {
		return nil, errors.New("pool size less than 1.")
	}

	if initialPoolSize > poolSize {
		initialPoolSize = poolSize
	}

	pool := &ThriftClientPool{
		Name:              name,
		Dial:              dialFn,
		Close:             closeFn,
		KeepAlive:         keepAliveFn,
		MaxPoolSize:       poolSize,
		KeepAliveInterval: DefaultKeepAliveInterval,
		DialRetryCount:    DefaultDialRetryCount,
		DialRetryInterval: DefaultRetryInterval,
	}

	pool.KeepAliveInterval = time.Second * 30
	pool.DialRetryInterval = time.Second * 30
	pool.retryPool = make(chan int, poolSize)
	pool.alivePool = make(chan interface{}, poolSize)
	pool.swapPool = make(chan interface{}, poolSize)

	for i := 0; i < initialPoolSize; i++ {
		if c, err := dialFn(pool.Name); err == nil {
			pool.alivePool <- c
		}
	}

	go pool.retryLoop()
	go pool.keepAliveLoop()

	return pool, nil
}

func (p *ThriftClientPool) Get() (connection interface{}, err error) {

	select {
	case <-time.After(p.CreateNewInterval):
		p.sync.Lock()
		defer p.sync.Unlock()

		log.Println("Get new connection from new create.")
		if int(p.workConnCount)+len(p.retryPool)+len(p.alivePool)+len(p.swapPool) < p.MaxPoolSize {

			retry := 0
			for retry < p.DialRetryCount {
				if connection, err = p.Dial(p.Name); err != nil {
					retry++
					continue
				} else {
					atomic.AddInt32(&p.workConnCount, 1)
					return
				}
			}

			if retry >= p.DialRetryCount {
				p.retryPool <- 0
				return nil, err
			}
		} else {
			return nil, errors.New(fmt.Sprintf("Pool Was Exhausted, detail: working: %v, alive: %v, retry: %v.", p.workConnCount, len(p.alivePool), len(p.retryPool)))
		}
	case connection = <-p.alivePool:
		log.Println("Get new connection from alive pool.")
		atomic.AddInt32(&p.workConnCount, 1)
		return
	case connection = <-p.swapPool:
		log.Println("Get new connection from swap pool.")
		atomic.AddInt32(&p.workConnCount, 1)
		return
	}

	return nil, errors.New("Get Connection Timeout")
}

func (p *ThriftClientPool) Put(connection interface{}) (err error) {

	p.sync.Lock()

	if connection != nil {
		if p.isStopped {
			p.Close(p.Name, connection)
		} else {
			if len(p.alivePool) < p.MaxPoolSize {
				p.alivePool <- connection
			}
		}
	}

	atomic.SwapInt32(&p.workConnCount, p.workConnCount-1)
	p.sync.Unlock()

	return
}

func (p *ThriftClientPool) Release() {
	p.sync.Lock()
	p.isStopped = true

	for connection := range p.alivePool {
		if err := p.Close(p.Name, connection); err != nil {
			log.Println("Release connection error: ", err)
		}

		atomic.SwapInt32(&p.workConnCount, p.workConnCount-1)
	}

	p.sync.Unlock()
}

func (p *ThriftClientPool) retryLoop() {

	log.Println("retry loop start.")

	retryCircle := 0
	for {
		select {
		case <-time.After(p.DialRetryInterval):

			retryCircle++
			max := len(p.retryPool)
			for i := 0; i < max; i++ {
				if connection, err := p.Dial(p.Name); err == nil {
					<-p.retryPool
					p.alivePool <- connection
					log.Println("Retry Pool Success, retryCircle: ", retryCircle)
				} else {
					log.Println("Retry Pool Failed, retryCircle: ", retryCircle)
				}
			}

			if p.isStopped {
				break
			}
		}
	}

	log.Println("retry loop end.")
}

func (p *ThriftClientPool) keepAliveLoop() {

	log.Println("keepAlive loop start.")
	retryCircle := 0

	for {
		select {
		case <-time.After(p.KeepAliveInterval):

			if len(p.alivePool) > 0 {
				// send keep alive message to each connection
				for connection := range p.alivePool {
					if err := p.KeepAlive(p.Name, connection); err == nil {
						log.Println("Keepalive Pool Success, retryCircle: ", retryCircle)
						p.swapPool <- connection
					} else {
						log.Println("Keepalive Pool Failed, retryCircle: ", retryCircle)
						p.retryPool <- 0
					}

					if len(p.alivePool) == 0 {
						break
					}
				}
			}

			if len(p.swapPool) > 0 {
				// restore alive connection pool.
				for connection := range p.swapPool {
					p.alivePool <- connection

					if len(p.swapPool) == 0 {
						break
					}
				}
			}
		}

		if p.isStopped {
			for connection := range p.alivePool {
				p.Close(p.Name, connection)
			}
			break
		}
	}

	log.Println("keepAlive loop end.")
}