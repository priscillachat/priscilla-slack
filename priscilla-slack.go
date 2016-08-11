package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/priscillachat/prisclient"
	"github.com/priscillachat/prislog"
	"golang.org/x/net/websocket"
	_ "gopkg.in/yaml.v2"
	"io/ioutil"
	"net/http"
	"os"
	"time"
)

const (
	SlackAPI = "https://slack.com/api/"
)

type slackStart struct {
	Ok       bool            `json:"ok"`
	Url      string          `json:"url"`
	Error    string          `json:"error"`
	Self     slackStartSelf  `json:"self"`
	Users    []*slackUser    `json:"users"`
	Channels []*slackChannel `json:"channels"`
}

type slackPing struct {
	ID   int    `json:"id"`
	Type string `json:"type"`
}

type slackStartSelf struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type slackChannel struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	IsMember bool   `json:"is_member"`
}

type userProfile struct {
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	RealName  string `json:"real_name"`
	Email     string `json:"email"`
}

type slackUser struct {
	ID      string      `json:"id"`
	Name    string      `json:"name"`
	Profile userProfile `json:"profile"`
}

type slackClient struct {
	name           string
	id             string
	token          string
	api            *http.Client
	ws             *websocket.Conn
	usersByName    map[string]*slackUser
	usersByID      map[string]*slackUser
	channelsByName map[string]*slackChannel
	channelsByID   map[string]*slackChannel
}

type slackMessage struct {
	ID        uint64 `json:"id"`
	Channel   string `json:"channel"`
	Type      string `json:"type"`
	User      string `json:"user"`
	Text      string `json:"text"`
	Timestamp string `json:"ts"`
}

var logger *prislog.PrisLog

func main() {
	token := flag.String("token", "", "slack bot token")
	server := flag.String("server", "127.0.0.1", "priscilla server")
	port := flag.String("port", "4517", "priscilla server port")
	sourceid := flag.String("id", "priscilla-slack", "source id")
	loglevel := flag.String("loglevel", "warn", "loglevel")
	secret := flag.String("secret", "abcdefg", "secret for priscilla server")
	logfile := flag.String("logfile", "STDOUT", "Log file")

	flag.Parse()

	var err error

	var logwriter *os.File

	if *logfile == "STDOUT" {
		logwriter = os.Stdout
	} else {
		logwriter, err = os.OpenFile(*logfile,
			os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
		if err != nil {
			fmt.Println("Unable to write to log file", *logfile, ":", err)
			os.Exit(1)
		}
		defer logwriter.Close()
	}

	logger, err = prislog.NewLogger(logwriter, *loglevel)

	if err != nil {
		fmt.Println("Error initializing logger: ", err)
		os.Exit(-1)
	}

	if *token == "" {
		logger.Error.Fatal("No token specified")
	}

	slack := &slackClient{
		token:          *token,
		usersByName:    make(map[string]*slackUser),
		usersByID:      make(map[string]*slackUser),
		channelsByName: make(map[string]*slackChannel),
		channelsByID:   make(map[string]*slackChannel),
	}

	priscilla, err := prisclient.NewClient(*server, *port, "adapter",
		*sourceid, *secret, true, logger)

	run(priscilla, slack)
}

func run(priscilla *prisclient.Client, slack *slackClient) {
	messageFromSlack := make(chan *slackMessage)
	go slack.listen(messageFromSlack)

	fromPris := make(chan *prisclient.Query)
	toPris := make(chan *prisclient.Query)
	go priscilla.Run(toPris, fromPris)

	keepAlive := make(chan bool)
	go slack.keepAlive(keepAlive)

	for {
		select {
		case msg := <-messageFromSlack:
			logger.Debug.Println("id:", msg.ID)
			logger.Debug.Println("type:", msg.Type)
			logger.Debug.Println("user:", msg.User)
			logger.Debug.Println("text:", msg.Text)
			logger.Debug.Println("channel:", msg.Channel)
			logger.Debug.Println("timestamp:", msg.Timestamp)
		case <-keepAlive:
			websocket.JSON.Send(slack.ws, slackPing{Type: "ping"})
		}
	}
}

func (slack *slackClient) connect() error {
	if slack.api == nil {
		slack.api = &http.Client{}
	}

	req, err := http.NewRequest("GET", SlackAPI+"rtm.start", nil)
	q := req.URL.Query()
	q.Add("token", slack.token)
	q.Add("simple_latest", "true")
	q.Add("no_unreads", "true")
	req.URL.RawQuery = q.Encode()

	resp, err := slack.api.Do(req)

	if err != nil {
		logger.Error.Println("Error calling start API:", err)
		return err
	}
	logger.Info.Println("rtm.start called")

	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		logger.Error.Println("API did not return 200:", resp.StatusCode)
		return errors.New("API did not return 200")
	}

	body, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		logger.Error.Println("Unable to read response", err)
		return err
	}

	var startObj slackStart

	err = json.Unmarshal(body, &startObj)

	if err != nil {
		logger.Error.Println("Unable to decode response", err)
		return err
	}

	if !startObj.Ok {
		logger.Error.Println("API returned error:", startObj.Error)
		return errors.New(startObj.Error)
	}

	slack.name = startObj.Self.Name
	slack.id = startObj.Self.ID

	slack.ws, err = websocket.Dial(startObj.Url, "", SlackAPI)

	if err != nil {
		logger.Error.Println("Error connecting to websocket:", err)
		return err
	}

	for _, channel := range startObj.Channels {
		slack.channelsByName[channel.Name] = channel
		slack.channelsByID[channel.ID] = channel

		logger.Debug.Println("Found channel:", *channel)
	}

	for _, user := range startObj.Users {
		slack.usersByName[user.Name] = user
		slack.usersByID[user.ID] = user

		logger.Debug.Println("Found user:", *user)
		logger.Debug.Println("User profile:", user.Profile)
	}

	return nil
}

func (slack *slackClient) disconnect() {
	if slack.ws != nil {
		slack.ws.Close()
	}
}

func (slack *slackClient) keepAlive(trigger chan<- bool) {
	for _ = range time.Tick(10 * time.Second) {
		trigger <- true
	}
}

func (slack *slackClient) listen(msgOut chan<- *slackMessage) {
	for err := slack.connect(); err != nil; err = slack.connect() {
		logger.Error.Println("Slack connect failure:", err)
		logger.Warn.Println("Sleeping 10 seconds before retry...")
		time.Sleep(10 * time.Second)
	}

	for {
		msg := new(slackMessage)
		websocket.JSON.Receive(slack.ws, msg)
		msgOut <- msg
	}
}
