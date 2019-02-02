package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io/ioutil"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/priscillachat/prisclient"
	"github.com/priscillachat/prislog"
	"gopkg.in/yaml.v2"
)

const (
	SlackAPI = "https://slack.com/api/"
)

type slackConnect struct {
	Ok    bool             `json:"ok"`
	URL   string           `json:"url"`
	Error string           `json:"error"`
	Self  slackConnectSelf `json:"self"`
}

type slackPing struct {
	ID   int    `json:"id"`
	Type string `json:"type"`
}

type slackConnectSelf struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type slackConversation struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsChannel bool   `json:"is_channel"`
	IsGroup   bool   `json:"is_group"`
	IsIm      bool   `json:"is_im"`
	IsMember  bool   `json:"is_member"`
	IsPrivate bool   `json:"is_private"`
	IsMpim    bool   `json:"is_mpim"`
}

type apiRespMetadata struct {
	NextCursor string `json:"next_cursor"`
}
type apiRespConversations struct {
	Ok            bool                 `json:"ok"`
	Conversations []*slackConversation `json:"channels"`
	RespMetadata  apiRespMetadata      `json:"response_metadata"`
	Error         string               `json:"error"`
}

type apiRespConversation struct {
	Ok           bool               `json:"ok"`
	Conversation *slackConversation `json:"channel"`
	Error        string             `json:"error"`
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

type apiRespUser struct {
	Ok    bool       `json:"ok"`
	User  *slackUser `json:"user"`
	Error string     `json:"error"`
}

type slackClient struct {
	name               string
	id                 string
	token              string
	api                *http.Client
	ws                 *websocket.Conn
	userByName         map[string]*slackUser
	userByID           map[string]*slackUser
	userByMention      map[string]*slackUser
	userByEmail        map[string]*slackUser
	conversationByName map[string]*slackConversation
	conversationByID   map[string]*slackConversation
	aMention           string
	messageCounter     uint64
}

type slackMessage struct {
	ID        uint64 `json:"id"`
	Channel   string `json:"channel"`
	Type      string `json:"type"`
	User      string `json:"user,omitempty"`
	Text      string `json:"text"`
	Timestamp string `json:"ts,omitempty"`
}

type config struct {
	Port     int                      `yaml:"port"`
	Secret   string                   `yaml:"secret"`
	Adapters map[string]adapterConfig `yaml:"adapters"`
}

type adapterConfig struct {
	Params map[string]*string `yaml:"params"`
}

var logger *prislog.PrisLog
var slack *slackClient
var priscilla *prisclient.Client

func init() {
	confFile := flag.String("conf", "",
		"Use Priscilla config file, command line overrides options inside")
	confName := flag.String("confname", "",
		"Name of the config subsection (under \"adapters\")")

	flag.Parse()

	// initialize sslack client
	slack = &slackClient{
		token:              "",
		userByName:         make(map[string]*slackUser),
		userByMention:      make(map[string]*slackUser),
		userByEmail:        make(map[string]*slackUser),
		userByID:           make(map[string]*slackUser),
		conversationByName: make(map[string]*slackConversation),
		conversationByID:   make(map[string]*slackConversation),
	}

	// place holder for decoding config file
	var conf config
	var err error

	if *confFile != "" && *confName != "" {
		confRaw, err := ioutil.ReadFile(*confFile)

		if err != nil {
			fmt.Fprintln(os.Stderr, "Error reading config file", err)
			os.Exit(1)
		}

		err = yaml.Unmarshal(confRaw, &conf)

		if err != nil {
			fmt.Fprintln(os.Stderr, "Error parsing config file", err)
			os.Exit(1)
		}
	} else {
		fmt.Fprintln(os.Stderr, "conf and confname are required")
		os.Exit(1)
	}

	fmt.Println("conf loaded:", conf)

	server := "127.0.0.1"
	port := "4517"
	sourceID := "priscilla-slack"
	secret := "abcdefg"
	loglevel := "warn"
	logfile := "STDOUT"

	if conf.Port != 0 {
		port = fmt.Sprintf("%d", conf.Port)
	}

	if conf.Secret != "" {
		secret = conf.Secret
	}

	if adapterConf, ok := conf.Adapters[*confName]; ok {
		for key, value := range adapterConf.Params {
			switch key {
			case "token":
				slack.token = *value
			case "server":
				server = *value
			case "sourceId":
				sourceID = *value
			case "loglevel":
				loglevel = *value
			case "logfile":
				logfile = *value
			}
		}
	}

	// log destination
	var logwriter *os.File

	if logfile == "STDOUT" {
		logwriter = os.Stdout
	} else {
		logwriter, err = os.OpenFile(logfile,
			os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
		if err != nil {
			fmt.Println("Unable to write to log file", logfile, ":", err)
			os.Exit(1)
		}
		defer logwriter.Close()
	}

	logger, err = prislog.NewLogger(logwriter, loglevel)

	if err != nil {
		fmt.Println("Error initializing logger:", err)
		os.Exit(-1)
	}

	logger.Debug.Println("end of initialization, slack:", slack)

	priscilla, err = prisclient.NewClient(server, port, "adapter",
		sourceID, secret, true, logger)

	if err != nil {
		logger.Error.Fatal("Error initializing priscilla client:", err)
	}
}

func main() {

	// start the client
	run()
}

func (slack *slackClient) String() string {
	return fmt.Sprint(
		"{",
		"name:", slack.name, ", ",
		"id:", slack.id, ", ",
		"token:", "[masked]", ", ",
		"api:", slack.api, ", ",
		"ws:", slack.ws, ", ",
		"aMention:", slack.aMention, ", ",
		"messageCounter:", slack.messageCounter,
		"}",
	)
}

func (conversation *slackConversation) String() string {
	return fmt.Sprint(
		"{",
		"id:", conversation.ID, ", ",
		"name:", conversation.Name, ", ",
		"is_channel:", conversation.IsChannel, ", ",
		"is_group:", conversation.IsGroup, ", ",
		"is_im:", conversation.IsIm, ", ",
		"is_member:", conversation.IsMember, ", ",
		"is_private:", conversation.IsPrivate, ", ",
		"is_mpim:", conversation.IsMpim,
		"}",
	)
}

func (user *slackUser) String() string {
	return fmt.Sprint(
		"{",
		"id:", user.ID, ", ",
		"name:", user.Name, ", ",
		"first_name:", user.Profile.FirstName, ", ",
		"last_name:", user.Profile.LastName, ", ",
		"real_name:", user.Profile.RealName, ", ",
		"email:", user.Profile.Email,
		"}",
	)
}

func (message *slackMessage) String() string {
	return fmt.Sprint(
		"{",
		"id:", message.ID, ", ",
		"channel:", message.Channel, ", ",
		"type:", message.Type, ", ",
		"user:", message.User, ", ",
		"text:", message.Text, ", ",
		"timestamp:", message.Timestamp, ", ",
	)
}

func run() {
	messageFromSlack := make(chan *slackMessage)
	go slack.listen(messageFromSlack)

	fromPris := make(chan *prisclient.Query)
	toPris := make(chan *prisclient.Query)
	go priscilla.Run(toPris, fromPris)

	keepAlive := make(chan bool)
	//go slack.keepAlive(keepAlive)

	for {
		select {
		case msg := <-messageFromSlack:
			logger.Debug.Println("raw:", msg)
			logger.Debug.Println("id:", msg.ID)
			logger.Debug.Println("type:", msg.Type)
			logger.Debug.Println("user:", msg.User)
			logger.Debug.Println("text:", msg.Text)
			logger.Debug.Println("channel:", msg.Channel)
			logger.Debug.Println("timestamp:", msg.Timestamp)
			if msg.Type == "message" {
				var chanName, userName string
				if msg.Channel != "" {
					if ch, ok := slack.conversationByID[msg.Channel]; ok {
						logger.Debug.Println("decoded channel:", ch.Name)
						chanName = ch.Name
					} else {
						err := slack.populateConversation(msg.Channel)
						if err == nil {
							chanName = slack.conversationByID[msg.Channel].Name
						}
					}
				}
				if msg.User != "" {
					if user, ok := slack.userByID[msg.User]; ok {
						logger.Debug.Println("decoded user:",
							user.Profile.RealName)
						userName = user.Profile.RealName
					} else {
						err := slack.populateUser(msg.User)
						if err == nil {
							userName = slack.userByID[msg.User].Profile.RealName
						}
					}
				}
				if chanName != "" && userName != "" {
					mentioned, err := regexp.MatchString(slack.aMention,
						msg.Text)
					if err != nil {
						logger.Error.Println("Error searching mention:", err)
					}

					slackUser := slack.userByID[msg.User]
					stripped :=
						strings.Replace(msg.Text, slack.aMention, "", -1)
					unescapedText := html.UnescapeString(stripped)

					clientQuery := prisclient.Query{
						Type: "message",
						To:   "server",
						Message: &prisclient.MessageBlock{
							Message:   msg.Text,
							From:      userName,
							Room:      chanName,
							Mentioned: mentioned,
							Stripped:  unescapedText,
							User: &prisclient.UserInfo{
								Id:      msg.User,
								Name:    userName,
								Mention: slackUser.Name,
								Email:   slackUser.Profile.Email,
							},
						},
					}

					toPris <- &clientQuery
				} else {
					logger.Info.Println(
						"Missing chanName or userName, not activating")
				}
			}
		case query := <-fromPris:
			logger.Debug.Println("Query received:", *query)
			switch query.Type {
			case "message":
				slack.sendMessage(query.Message)
			}
		case <-keepAlive:
			slack.ws.WriteJSON(slackPing{Type: "ping"})
		}
	}
}

func (slack *slackClient) connect() error {
	if slack.api == nil {
		slack.api = &http.Client{}
	}

	req, err := http.NewRequest("GET", SlackAPI+"rtm.connect", nil)
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

	var connectObject slackConnect

	err = json.Unmarshal(body, &connectObject)

	if err != nil {
		logger.Error.Println("Unable to decode response", err)
		return err
	}

	if !connectObject.Ok {
		logger.Error.Println("API returned error:", connectObject.Error)
		return errors.New(connectObject.Error)
	}

	slack.name = connectObject.Self.Name
	slack.id = connectObject.Self.ID
	slack.aMention = "<@" + slack.id + ">"

	logger.Debug.Println("Bot name:", slack.name)
	logger.Debug.Println("Bot ID:", slack.id)

	slack.ws, _, err = websocket.DefaultDialer.Dial(connectObject.URL, nil)

	if err != nil {
		logger.Error.Println("Error connecting to websocket:", err)
		return err
	}

	slack.populateConversations()

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
	var err error
	init := true
	for {
		if init || err != nil {
			for err := slack.connect(); err != nil; err = slack.connect() {
				logger.Error.Println("Slack connect failure:", err)
				logger.Warn.Println("Sleeping 10 seconds before retry...")
				time.Sleep(60 * time.Second)
			}
			init = false
		}

		msg := new(slackMessage)
		err = slack.ws.ReadJSON(msg)
		if err == nil {
			msgOut <- msg
		} else {
			logger.Error.Println("Websocket error, wait 60s before reconnect:",
				err)
			time.Sleep(60 * time.Second)
		}

	}
}

func (slack *slackClient) apiGet(path string,
	params map[string]string) ([]byte, error) {
	if slack.api == nil {
		slack.api = &http.Client{}
	}

	req, err := http.NewRequest("GET", SlackAPI+path, nil)
	q := req.URL.Query()
	q.Add("token", slack.token)

	for name, value := range params {
		q.Add(name, value)
	}

	req.URL.RawQuery = q.Encode()

	resp, err := slack.api.Do(req)

	if err != nil {
		logger.Error.Println("Error calling slack API [", path, "]:", err)
		return []byte{}, err
	}

	logger.Debug.Println("API called:", path)

	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		logger.Error.Println("Not 200 [", path, "]:", resp.StatusCode)
		return []byte{}, errors.New("API did not return 200")
	}

	return ioutil.ReadAll(resp.Body)
}

func (slack *slackClient) populateUser(id string) error {
	body, err := slack.apiGet("users.info", map[string]string{"user": id})

	if err != nil {
		logger.Error.Println("Error retrieving user during API call:", err)
		return err
	}

	var apiResp apiRespUser

	err = json.Unmarshal(body, &apiResp)

	if err != nil {
		logger.Error.Println("Unable to decode user info response:", err)
		return err
	}

	if !apiResp.Ok {
		logger.Error.Println("API returned error:", apiResp.Error)
	}

	logger.Debug.Println("Found user:", apiResp.User)
	slack.userByName[apiResp.User.Profile.RealName] = apiResp.User
	slack.userByID[apiResp.User.ID] = apiResp.User
	slack.userByMention[apiResp.User.Name] = apiResp.User
	if apiResp.User.Profile.Email != "" {
		slack.userByEmail[apiResp.User.Profile.Email] = apiResp.User
	}

	return nil
}
func (slack *slackClient) populateConversation(id string) error {
	body, err := slack.apiGet("conversations.info",
		map[string]string{"channel": id})

	if err != nil {
		logger.Error.Println("Error retrieving user during API call:", err)
		return err
	}

	var apiResp apiRespConversation

	err = json.Unmarshal(body, &apiResp)

	if err != nil {
		logger.Error.Println("Unable to decode conversation info response:", err)
		return err
	}

	if !apiResp.Ok {
		logger.Error.Println("API returned error:", apiResp.Error)
		return err
	}

	logger.Debug.Println("Found conversation:", apiResp.Conversation)

	slack.conversationByName[apiResp.Conversation.Name] = apiResp.Conversation
	slack.conversationByID[apiResp.Conversation.ID] = apiResp.Conversation

	return nil
}
func (slack *slackClient) populateConversations() error {

	request := map[string]string{"limit": "1000"}
	for {
		body, err := slack.apiGet("conversations.list", request)
		if err != nil {
			logger.Error.Println(
				"Error listing conversations during API call:", err)
			return err
		}

		apiResp := new(apiRespConversations)

		err = json.Unmarshal(body, apiResp)

		if err != nil {
			logger.Error.Println("Unable to decode conversation list:", err)
			return err
		}

		if !apiResp.Ok {
			logger.Error.Println("conversation API returned error:",
				apiResp.Error)
			return err
		}

		for _, conv := range apiResp.Conversations {
			logger.Debug.Println("Found conversation:", conv)
			slack.conversationByName[conv.Name] = conv
			slack.conversationByID[conv.ID] = conv
		}

		if apiResp.RespMetadata.NextCursor == "" {
			return nil
		}
		request["cursor"] = apiResp.RespMetadata.NextCursor

	}
}

func (slack *slackClient) sendMessage(message *prisclient.MessageBlock) error {

	slack.messageCounter++

	slackMsg := slackMessage{
		ID:      slack.messageCounter,
		Channel: slack.conversationByName[message.Room].ID,
		Type:    "message",
		Text:    message.Message,
	}

	if len(message.MentionNotify) > 0 {
		for _, name := range message.MentionNotify {
			logger.Debug.Println("Requested to mention:", name)
			if user, ok := slack.userByName[name]; ok {
				logger.Debug.Println("Mention user found:", user.Name)
				slackMsg.Text += " <@" + user.ID + ">"
			}
		}
	}

	return slack.ws.WriteJSON(&slackMsg)
}
