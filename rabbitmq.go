package gorabbit

import (
	"errors"
	"fmt"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"
)

type RabbitMQ interface {
	Connect() error
	Close()
	Conn() *amqp.Connection
	Channel() *amqp.Channel
	NewScheduledJob(handler JobHandler, jobExchange string, jobQueue string, autoAck bool) ScheduledJob
	StartJobs()
	ShutdownJobs()
	Declare(exchangeOpt Opt, queueOpts ...Opt) error
}

type rabbitMQ struct {
	dsn         string
	conn        *amqp.Connection
	channel     *amqp.Channel
	dialTimeout time.Duration
	dialRetry   uint
	ctxTimeout  time.Duration
	jobs        []*job
}

func NewRabbitMQ(
	dsn string,
	dialTimeout time.Duration,
	dialRetry uint,
	ctxTimeout time.Duration,
	logLevel string,
) RabbitMQ {
	if logLevel == "" {
		logLevel = "info"
	}
	logLevel = strings.ToLower(logLevel)
	InitLogger(logLevel)

	return &rabbitMQ{
		dsn:         dsn,
		dialTimeout: dialTimeout,
		dialRetry:   dialRetry,
		ctxTimeout:  ctxTimeout,
	}
}

func (r *rabbitMQ) Close() {
	err := r.channel.Close()
	if err != nil {
		logger.Error("error in closing rabbitmq channel", zap.Error(err))
	}

	err = r.conn.Close()
	if err != nil {
		logger.Error("error in closing rabbitmq connection", zap.Error(err))
	}
}

func (r *rabbitMQ) Connect() error {
	var err error

	counter := uint(0)
	ticker := time.NewTicker(r.dialTimeout)
	defer ticker.Stop()
	logger.Warn("try to connect to rabbitMQ")
	for time.Now(); true; <-ticker.C {
		counter++

		r.conn, err = amqp.Dial(r.dsn)
		if err == nil {
			break
		}

		if counter >= r.dialRetry {
			return fmt.Errorf("failed to connect a RabbitMQ channel: %s", err)
		}
	}

	go func() {
		<-r.conn.NotifyClose(make(chan *amqp.Error))
		logger.Warn(">>>>>>>>> try to reconnect to rabbitmq")
		err := r.Connect()
		if err != nil {
			logger.Fatal("error in connecting to rabbitMQ", zap.Error(err))
		}

		r.ShutdownJobs()
		r.StartJobs()
		if err != nil {
			logger.Fatal("error in starting jobs via rabbitMQ", zap.Error(err))
		}
		logger.Warn("all jobs restarted")
	}()

	r.channel, err = r.conn.Channel()
	if err != nil {
		return fmt.Errorf("failed to open a RabbitMQ channel: %s", err)
	}

	return nil
}

func (r *rabbitMQ) Conn() *amqp.Connection {
	return r.conn
}

func (r *rabbitMQ) Channel() *amqp.Channel {
	return r.channel
}

func (r *rabbitMQ) Declare(exchangeOpt Opt, queueOpts ...Opt) error {
	if len(queueOpts) == 0 {
		return errors.New("length of queue options is zero")
	}

	exchangeOption := exchangeOpt().(exchangeDeclareOpt)
	err := r.Channel().ExchangeDeclare(
		exchangeOption.name,
		exchangeOption.kind,
		exchangeOption.durable,
		exchangeOption.autoDelete,
		exchangeOption.internal,
		exchangeOption.noWait,
		exchangeOption.args,
	)
	if err != nil {
		return err
	}

	for _, o := range queueOpts {
		option := o().(queueDeclareOpt)
		queue, err := r.Channel().QueueDeclare(
			option.name,
			option.durable,
			option.autoDelete,
			option.exclusive,
			option.noWait,
			option.args,
		)
		if err != nil {
			return err
		}

		err = r.Channel().QueueBind(
			queue.Name,
			option.name,
			exchangeOption.name,
			option.noWait,
			option.args,
		)
		if err != nil {
			return err
		}
	}

	return nil
}

func (r *rabbitMQ) StartJobs() {
	for _, j := range r.jobs {
		err := j.Consume(r.ctxTimeout)
		if err != nil {
			logger.Fatal("error in starting job", zap.Error(err))
		}
	}
}

func (r *rabbitMQ) NewScheduledJob(
	handler JobHandler,
	jobExchange string,
	jobQueue string,
	autoAck bool,
) ScheduledJob {
	j := &job{
		channel:     r.channel,
		handler:     handler,
		jobExchange: jobExchange,
		jobQueue:    jobQueue,
		shutdown:    make(chan struct{}),
		autoAck:     autoAck,
	}

	r.jobs = append(r.jobs, j)

	return j
}
