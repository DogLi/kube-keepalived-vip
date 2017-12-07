package controller

import (
	"bytes"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"github.com/bitly/go-simplejson"
	"github.com/golang/glog"
	"github.com/satori/go.uuid"
	"github.com/streadway/amqp"
	"strings"
	"sync"
	"time"
)

var ipMutex sync.Mutex

func (self *ZStack) BuildData() amqp.Publishing {
	msgId := newUuid()
	jsonStr := fmt.Sprintf(`{
"org.zstack.header.ecfs.APIQueryEcfsHealthMsg":
{"headers": {
		"replyTo": "%s",
		"noReply": "false",
		"correlationId": "%s"
	},
"serviceId": "api.portal",
"session": {"uuid": "%s"},
"id": "%s"}
}
`, self.replyQueueName, msgId, self.session.token, msgId)
	glog.Infof("Build message: %s", jsonStr)
	msg := self.encodeMessage(msgId, jsonStr)
	return msg
}

func (ipvsc *ipvsControllerController) AcquireVip() (string, error) {
	msgId := newUuid()
	localIp := ipvsc.keepalived.ip
	localMask := ipvsc.keepalived.netmask
	jsonStr := fmt.Sprintf(`{
"org.zstack.header.ecfs.APIQueryEcfsHealthMsg":
{"headers": {
		"replyTo": "%s",
		"noReply": "false",
		"correlationId": "%s"
	},
"serviceId": "api.portal",
"session": {"uuid": "%s"},
"id": "%s",
"localip": "%s",
"localMask": "%s",
"type": "GetIP"
}
}`, ipvsc.zstack.replyQueueName, msgId, ipvsc.zstack.session.token, msgId, localIp, localMask)
	ipvsc.zstack.UpdateSession()
	msg := ipvsc.zstack.encodeMessage(msgId, jsonStr)
	// TODO: get ip
	jsonResult, err := ipvsc.zstack.SendMsg(msg)
	if err != nil {
		glog.Errorf("failed to get ip from zstack: %s", err)
	}
	glog.Infof("jsonResult: %s", jsonResult)
	ip := "10.10.40.44"
	return ip, nil
}

func (ipvsc *ipvsControllerController) ReleaseVip(vip string) error {
	msgId := newUuid()
	jsonStr := fmt.Sprintf(`{
"org.zstack.header.ecfs.APIQueryEcfsHealthMsg":
{"headers": {
		"replyTo": "%s",
		"noReply": "false",
		"correlationId": "%s"
	},
"serviceId": "api.portal",
"session": {"uuid": "%s"},
"id": "%s",
"ip": "%s",
"type": "ReleaseIP"
}
}`, ipvsc.zstack.replyQueueName, msgId, ipvsc.zstack.session.token, msgId, vip)
	ipvsc.zstack.UpdateSession()
	msg := ipvsc.zstack.encodeMessage(msgId, jsonStr)
	jsonResult, err := ipvsc.zstack.SendMsg(msg)
	if err != nil{
		glog.Errorf("Release vip %s failed: %s", vip, err)
	}
	// TODO: get the result from jsonResult
	glog.Infof("%s", jsonResult)
	return nil
}

var (
	P2P_EXCHANGE   = "P2P"
	API_SERVICE_ID = "zstack.message.api.portal"
	QUEUE_PREFIX   = "zstack.ui.message.%s"
)

type Session struct {
	timeTag   time.Time
	available bool
	token     string
	timeout   time.Duration
}

type ZStack struct {
	uuid string
	// a list of RabbitMQ url. A single url is in format of "
	// "account:password@ip:port/virtual_host_name. Multiple urls are split by ','.
	urlStrings     string
	P2P_EXCHANGE   string
	replyQueueName string
	conn           *amqp.Connection
	session        Session
}

func newUuid() string {
	uuid := uuid.NewV4()
	glog.Infof("create uuid: %s", uuid)
	return uuid.String()
}
func NewZstack(urlString string) *ZStack {
	uuid := newUuid()
	self := ZStack{
		urlStrings:     urlString,
		uuid:           uuid,
		P2P_EXCHANGE:   P2P_EXCHANGE,
		replyQueueName: fmt.Sprintf(QUEUE_PREFIX, uuid),
	}
	self.session = Session{
		timeout:   2 * time.Hour,
		available: false,
	}

	return &self
}

func (self *ZStack) Connect() error {
	c := make(chan *amqp.Error)
	urls := strings.Split(self.urlStrings, ",")
	go func() {
		err := <-c
		glog.Infof("reconnect: %s", err.Error())
		self.Connect()
	}()
	// connect to rabbitmq
	var err error
	for _, url := range urls {
		url := fmt.Sprintf("amqp://%s", url)
		glog.Infof("connect to rabbitmq: %s", url)
		self.conn, err = amqp.Dial(url)
		if err == nil {
			break
		}
	}

	if err != nil {
		return err
	}
	// create queue
	if err = self.createQueue(); err != nil {
		return err
	}
	// get token
	if err = self.UpdateSession(); err != nil {
		return err
	} else {
		glog.Infof("update session success!")
	}
	self.conn.NotifyClose(c)
	return nil
}

func (self *ZStack) UpdateSession() error {
	timeNow := time.Now()
	if timeNow.Sub(self.session.timeTag) < self.session.timeout && self.session.available {
		glog.Infof("session is avaliable, no need to update!")
		return nil
	}
	msgId := newUuid()
	user := "admin"
	sha := sha512.New()
	sha.Write([]byte("password"))
	password := hex.EncodeToString(sha.Sum(nil))
	jsonStr := fmt.Sprintf(`{
  "org.zstack.header.identity.APILogInByAccountMsg": {
	"headers": {
		"replyTo": "%s",
		"noReply": "false",
		"correlationId": "%s"
	  },
    "serviceId": "api.portal",
    "accountName": "%s",
    "id": "%s",
    "password": "%s"
  }
}`, self.replyQueueName, msgId, user, msgId, password)

	glog.Infof("send msg: %s", jsonStr)
	glog.Infof("session timeout, update it!")
	data := self.encodeMessage(msgId, jsonStr)
	result, err := self.SendMsg(data)
	if err != nil {
		glog.Errorf("login failed: %s", err)
		return err
	}
	// decode data
	body := result.Get("org.zstack.header.identity.APILogInReply")
	success := body.Get("success").MustBool(false)
	errMsg := body.Get("error").Get("description").MustString("get ")

	if success {
		token := body.Get("inventory").Get("uuid").MustString()
		self.session.token = token
		self.session.available = true
		self.session.timeTag = timeNow
		return nil
	} else {
		self.session.available = false
	}

	return fmt.Errorf("%s", errMsg)
	self.session.timeTag = timeNow
	glog.Infof("%s", result)
	return nil
}

func (self *ZStack) createQueue() error {
	ch, err := self.conn.Channel()
	if err != nil {
		return err
	}
	// create queue
	_, err = ch.QueueDeclare(
		self.replyQueueName,          // name
		false,               // durable
		true,             // delete when unused
		false,             // exclusive
		false,                // no-wait
		nil,                 	  // arguments
	)
	if err != nil {
		return err
	} else {
		glog.Infof("create queue %s success!", self.replyQueueName)
	}

	// bind queue to P2P exchange
	err = ch.QueueBind(self.replyQueueName, self.replyQueueName, self.P2P_EXCHANGE, false, nil)
	return err
}

func (self *ZStack) SendMsg(msg amqp.Publishing) (*simplejson.Json, error) {
	channel, err := self.conn.Channel()
	if err != nil {
		glog.Errorf("get channel failed: %s", err)
		return &simplejson.Json{}, err
	}
	// receive message
	msgs, err := channel.Consume(
		self.replyQueueName,
		"",
		false,
		false,
		false,
		false,
		nil)

	// send message
	err = channel.Publish(
		self.P2P_EXCHANGE,
		API_SERVICE_ID,
		false,
		false,
		msg)

	if err != nil {
		glog.Errorf("send msg failed: %s", err)
		return &simplejson.Json{}, err
	} else {
		glog.Infof("send msg success: %s", msg.CorrelationId)
	}

	// timeout
	timeout := make(chan bool, 1)
	go func() {
		time.Sleep(8 * time.Second) // sleep one second
		timeout <- true
	}()

	var result string

L:
	for {
		select {
		case d := <-msgs:
			if result, err = self.decodeMessage(d, msg); err == nil {
				d.Ack(false)
				break L
			}
		case <-timeout:
			return &simplejson.Json{}, fmt.Errorf("get message %s from rabbitmq timeout!", msg.CorrelationId)
		}
	}

	js, err := simplejson.NewJson([]byte(result))
	return js, err
}

func (self *ZStack) Close() {
	self.conn.Close()
}

func (rmq *ZStack) decodeMessage(recive amqp.Delivery, msg amqp.Publishing) (string, error) {
	glog.Infof("receive message %s : %s", recive.Headers["correlationId"], recive.Body)
	if recive.Headers["correlationId"] == msg.Headers["correlationId"] {
		// bytes to string
		buffer := bytes.NewBuffer(recive.Body)
		s := buffer.String()
		return s, nil
	}
	return "", fmt.Errorf("correlationId not match")
}

func (rmq *ZStack) encodeMessage(msgId, msgContent string) amqp.Publishing {
	data := amqp.Publishing{
		ContentType:   "application/json",
		Body:          []byte(msgContent),
		CorrelationId: msgId,
	}
	return data
}
