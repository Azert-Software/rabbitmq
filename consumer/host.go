package consumer

import (
	"github.com/streadway/amqp"
	"context"
	log "github.com/sirupsen/logrus"
	"os"
	"os/signal"
	"syscall"
	"errors"
	"runtime/debug"
	"time"
	"fmt"
	"github.com/pborman/uuid"
	"sync"
)

// HostConfig contains global config
// used for the rabbit connection
type HostConfig struct{
	Address string
}

// Host is the container which is used
// to host all consumers that are registered.
// It is responsible for the amqp connection
// starting & gracefully stopping all running consumers
// h := NewRabbitHost().Init(cfg.Host)
// h.AddBroker(NewBroker(cfg.Exchange, [])
type Host interface{
	// AddBroker will register an exchange and n consumers
	// which will consume from that exchange
	AddBroker(context.Context, *ExchangeConfig, []Consumer) error
	// Start will setup all queues and routing keys
	// assigned to each consumer and then in turn start them
	Run(context.Context) (err error)
	// Middleware can be used to implement custom
	// middleware which gets called before messages
	// are passed to handlers
	Middleware(...HostMiddleware)
	// Stop can be called when you wish to shut down the host
	Stop(context.Context) error

	GetConnectionStatus() bool
}

type RabbitHost struct{
	c *HostConfig
	connection *amqp.Connection
	exchanges []Exchange
	channels map[string]*amqp.Channel
	middleware MiddlewareList
	connectionClose chan *amqp.Error
	wg *sync.WaitGroup
	mu *sync.Mutex
	connected bool
	shutdown bool
}

type Exchange struct{
	exchange *ExchangeConfig
	consumers []Consumer
}

// Init sets up the initial connection & quality of service
// to be used by all registered consumers
func NewConsumerHost(cfg *HostConfig) Host{
	host := &RabbitHost{
		exchanges:make([]Exchange, 0),
		channels:make(map[string]*amqp.Channel),
		c: cfg,
		connectionClose:make(chan *amqp.Error),
		wg: &sync.WaitGroup{},
		connected:false,
		mu: &sync.Mutex{},
	}
	go host.connectionLoop()
	host.connectionClose <- amqp.ErrClosed
	return host
}

// AddBroker will register an exchange and n consumers
// which will consume from that exchange
func (h *RabbitHost) AddBroker(ctx context.Context, cfg *ExchangeConfig, consumers []Consumer) error {
	h.exchanges = append(h.exchanges, Exchange{exchange:cfg, consumers:consumers})

	return nil
}

// Start will setup all queues and routing keys
// assigned to each consumer and then in turn start them
func (h *RabbitHost) Run(ctx context.Context) (err error){
	for h.connection == nil{}
	ch, err := h.connection.Channel()
	if err != nil{
		log.Errorf("error when getting channel from connection: %v", err.Error())
		return err
	}
	for _, b := range h.exchanges {
		n, err := b.exchange.GetName()
		if err != nil {
			log.Error(err)
			return err
		}
		b.exchange.BuildExchange(ch)

		go func() {
			for _, c := range b.consumers {
				cfg, err := c.Init()
				if err != nil {
					log.Fatal(err)
				}
				if cfg == nil {
					cfg = &ConsumerConfig{}
				}


				for k, r := range c.Queues(ctx){
					go func(key string, routes *Routes) {
						h.wg.Add(1)
						defer h.wg.Done()

						for {
							// wait until we have a connection
							if !h.connected {
								time.Sleep(200 * time.Millisecond)
								continue
							}

							// attempt to get a channel
							queueChannel, err := h.connection.Channel()
							if err != nil{
								log.Error("error setting up consumer queue for %s", key)
								time.Sleep(500 *time.Millisecond)
								continue
							}

							h.mu.Lock()
							h.channels[key] = queueChannel
							h.mu.Unlock()

							closeChannel := make(chan *amqp.Error)
							cancelChannel := make(chan string)
							queueChannel.NotifyClose(closeChannel)
							queueChannel.NotifyCancel(cancelChannel)

							// build the queue, if it's deleted it will be recreated
							cfg.BuildQueue(key, routes, queueChannel, n)

							// start consuming
							go func() {
								h.wg.Add(1)
								defer h.wg.Done()
								// start consuming messages
								msgs, err := queueChannel.Consume(key, fmt.Sprintf("%s-%s", cfg.GetName(), uuid.NewUUID()), false, cfg.GetExclusive(), false, cfg.GetNoWait(), cfg.Args)
								if err != nil {
									log.Fatal(err)
								}

								// setup global, consumer & default middleware
								middleware := h.buildChain(c.Middleware(errorHandler(routes.DeliveryFunc)), h.middleware)
								for d := range msgs {
									panicHandler(middleware).HandleMessage(context.Background(), d)
								}
							}()

							select {
								case queueErr := <-closeChannel:
									if h.shutdown{
										// indicates a graceful shutdown
										// exit the routine
										return
									} else if queueErr != nil{
										// there was an error, usually due to connection being closed
										// log it and then we attempt to recreate the channel & queue
										log.Errorf("queue channel closed for queue %s: %s", k, queueErr.Error())
									}
								case <-cancelChannel:
									if h.shutdown {
										return
									}
									log.Infof("channel for queue %s deleted, recreating", k)
							}
							h.mu.Lock()
							delete(h.channels, key)
							h.mu.Unlock()
						}
					}(k, r)

					// setup the dead letter queue
					if cfg.GetHasDeadletter() {
						// check queue every second to check it hasn't been deleted,
						// recreate it if we can
						go func(key string, routes *Routes){
							h.wg.Add(1)
							defer h.wg.Done()
							for {
								// we're in the middle of shutdown, exit
								if h.shutdown{
									return
								}
								// wait for connection
								for !h.connected{
									time.Sleep(200 *time.Millisecond)
								}

								t := time.NewTimer(time.Second)
								<-t.C

								dlCh, err := h.connection.Channel()
								if err != nil{
									log.Error(err)
									time.Sleep(200 *time.Millisecond)
									break
								}
								if err := cfg.BuildDeadletterQueue(routes, dlCh, h.connection, n); err != nil{
									log.Error(err)
								}
								t.Reset(time.Second)
							}
						}(k, r)
					}
				}
			}
		}()
	}

	ch.Close() // discard the setup channel
	log.Infof("host started")
	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	return h.Stop( ctx)
}

func (h *RabbitHost) Middleware(fn ...HostMiddleware) {
	h.middleware = append(h.middleware, fn...)
}

func (h *RabbitHost) Stop(context.Context) error{
	log.Infof("shutting down host")
	h.shutdown = true
	h.connected = false
	h.mu.Lock()
	for k, v := range h.channels{
		log.Infof("closing channel %s", k)
		if err := v.Close(); err != nil{
			log.Errorf("error when closing channel %s: %s",k, err)
			continue
		}
		log.Infof("channel for queue %s closed successfully", k)
	}
	h.mu.Unlock()
	h.wg.Wait()

	err := h.connection.Close()
	log.Infof("shutdown completed")
	return err
}

func (h *RabbitHost) GetConnectionStatus() bool {
	return h.connected
}

// panicHandler intercepts panics from a consumer, logs
// the error and stack trace then nacks the message
func panicHandler(h HandlerFunc) HandlerFunc{
	return func(ctx context.Context, d amqp.Delivery) {
		var err error
		defer func() {
			r := recover()
			if r != nil {
				switch t := r.(type) {
				case string:
					err = errors.New(t)
				case error:
					err = t
				default:
					err = errors.New("unknown error")
				}
				log.Errorf("panic handler recovered from unexpected panic, error: %s", err)
				log.Debugf("stack: %s", debug.Stack())
				d.Nack(false, false)
			}
		}()

		h(ctx, d)

	}
}

// errorHandler performs two functions
// it handles an error and returns an Ack if nil or a
// nack if err is not nil
// It also converts a KeyHandlerFunc to a HandlerFunc
// so middleware can be chained
func errorHandler(h KeyHandlerFunc) HandlerFunc{
	return func(ctx context.Context, d amqp.Delivery){
		err := h(ctx, d)
		if err != nil {
			log.Infof("error sending message with key %s and correlationid %v. Error: %s", d.RoutingKey, d.CorrelationId, err.Error())
			d.Nack(false, false)
		} else{
			d.Ack(false)
		}
	}
}

type HostMiddleware func(handler HandlerFunc) HandlerFunc

type MiddlewareList []HostMiddleware

func (h *RabbitHost) connect(){
	for {
		conn, err := amqp.Dial(h.c.Address)

		if err == nil {
			h.connected = true
			h.connection = conn
			log.Infof("connected to %s successful\n", h.c.Address)
			break
		}

		log.Error(err)
		log.Infof("Trying to reconnect to RabbitMQ at %s\n", h.c.Address)
		time.Sleep(200 * time.Millisecond)
	}
}

func (h *RabbitHost) connectionLoop() {
	var rabbitErr *amqp.Error

	for {
		rabbitErr = <-h.connectionClose
		if rabbitErr != nil {
			log.Errorf("connection lost %s", rabbitErr.Error())
			h.connected = false
			log.Infof("connecting to %s\n", h.c.Address)

			h.connect()
			h.connectionClose = make(chan *amqp.Error)
			for h.connection == nil {
				time.Sleep(200 *time.Millisecond)
			}
			h.connection.NotifyClose(h.connectionClose)
		}
	}
}


// buildChain builds the middleware chain recursively, functions are first class
func (h *RabbitHost) buildChain(f HandlerFunc, m MiddlewareList) HandlerFunc {
	// if our chain is done, use the original handlerfunc
	if len(m) == 0 {
		return f
	}
	// otherwise nest the handlerfuncs
	return m[0](h.buildChain(f, m[1:cap(m)]))
}
