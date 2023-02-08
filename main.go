package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/peterhellberg/giphy"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	awstrace "gopkg.in/DataDog/dd-trace-go.v1/contrib/aws/aws-sdk-go-v2/aws"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
	yaml "gopkg.in/yaml.v2"
)

// basic config unit
type instanceDef struct {
	Name              string        `yaml:"name"`
	Email             string        `yaml:"email"`
	OffTime           string        `yaml:"offtime"`
	OffTimeDuration   time.Duration `yaml:"offtimeduration"`
	OffDays           []string      `yaml:"offdays"`
	OnTime            string        `yaml:"ontime"`
	OnTimeDuration    time.Duration `yaml:"ontimeduration"`
	Override          bool          `yaml:"override"`
	OverrideTime      time.Time     `yaml:"overridetime"`
	UserName          string        `yaml:"username"`
	WarningIssued     bool          `yaml:"warningissued"`
	WarningIssuedTime time.Time     `yaml:"warningissuedtime"`
}

// for creating AppHome status pages
type UserStatus struct {
	OnTime   string
	OffTime  string
	OffDays  []string
	Override bool
	Status   string
	ImageUrl string
}

// map of all instances
type instancesConfig map[string]instanceDef

var api *slack.Client
var client *socketmode.Client
var instanceConfig = make(instancesConfig)
var svc *ec2.Client
var checkFrequency = 5 * time.Second
var stateFile string

//go:embed appHomeViewsAssets/*
var appHomeAssets embed.FS

// read stateFile
func (c *instancesConfig) readState() (ic instancesConfig) {
	yamlFile, err := os.ReadFile(stateFile)
	if err != nil {
		log.Print("no statefile found, creating new one")
		err := c.writeState()
		if err != nil {
			log.Print("error writing statefile")
		}
		return *c
	}
	err = yaml.Unmarshal(yamlFile, &ic)
	if err != nil {
		log.Printf("error reading state, returning config: %s", err)
	}
	return ic
}

// write stateFile
func (c *instancesConfig) writeState() error {
	log.Printf("INFO: writing state: %v", c)
	yamlData, err := yaml.Marshal(&c)
	if err != nil {
		fmt.Printf("Error while Marshaling. %v", err)
	}
	fileName := stateFile
	err = ioutil.WriteFile(fileName, yamlData, 0644)
	if err != nil {
		return err
	}
	return nil
}

func makeFun() (url string) {
	g := giphy.DefaultClient

	if thing, err := g.Random([]string{"silly cat"}); err == nil {
		url = thing.Data.MediaURL()
		return url
	}
	if trending, err := g.Trending(); err == nil {
		for i, d := range trending.Data {
			fmt.Println(i, "-", d.MediaURL())
		}
	}
	return ""
}

// setup config
func init() {
	stateFile = os.Getenv("STATE_FILE")
	if stateFile == "" {
		stateFile = "state.yaml"
	}
	appToken := os.Getenv("SLACK_APP_TOKEN")
	if appToken == "" {
		log.Fatal("missing SLACK_APP_TOKEN")
	}
	if !strings.HasPrefix(appToken, "xapp-") {
		fmt.Fprintf(os.Stderr, "SLACK_APP_TOKEN must have the prefix \"xapp-\".")
	}
	botToken := os.Getenv("SLACK_BOT_TOKEN")
	if botToken == "" {
		fmt.Fprintf(os.Stderr, "SLACK_BOT_TOKEN must be set.\n")
		os.Exit(1)
	}
	if !strings.HasPrefix(botToken, "xoxb-") {
		fmt.Fprintf(os.Stderr, "SLACK_BOT_TOKEN must have the prefix \"xoxb-\".")
	}
	api = slack.New(
		botToken,
		slack.OptionDebug(false),
		slack.OptionLog(log.New(os.Stdout, "api: ", log.Lshortfile|log.LstdFlags)),
		slack.OptionAppLevelToken(appToken),
	)
	client = socketmode.New(
		api,
		socketmode.OptionDebug(false),
		socketmode.OptionLog(log.New(os.Stdout, "socketmode: ", log.Lshortfile|log.LstdFlags)),
	)
	// read config file
	yamlFile, err := ioutil.ReadFile("config.yaml")
	if err != nil {
		log.Fatalf("unable to read secret map file: %s", err)
	}
	err = yaml.Unmarshal(yamlFile, &instanceConfig)
	if err != nil {
		log.Fatal(err)
	}
	// get on/offtimes
	for _, x := range instanceConfig {
		onTimeDuration, err := time.ParseDuration(x.OnTime)
		if err != nil {
			log.Fatal(err)
		}
		offTimeDuration, err := time.ParseDuration(x.OffTime)
		if err != nil {
			log.Fatal(err)
		}
		// update config with on/off time durations
		if entry, ok := instanceConfig[x.UserName]; ok {
			entry.OnTimeDuration = onTimeDuration
			entry.OffTimeDuration = offTimeDuration
			// update instanceConfig
			instanceConfig[x.UserName] = entry
		} else {
			log.Fatalf("could not set on/off durations for %v", x)
		}
		existingstate := instanceConfig.readState()
		// import existing overrides and warnings from state
		for _, x := range existingstate {
			if entry, ok := instanceConfig[x.UserName]; ok {
				entry.Override = x.Override
				entry.OverrideTime = x.OverrideTime
				entry.WarningIssued = x.WarningIssued
				entry.WarningIssuedTime = x.WarningIssuedTime
				// update instanceConfig
				instanceConfig[x.UserName] = entry

			}
		}
		err = instanceConfig.writeState()
		if err != nil {
			log.Fatal(err)
		}
		// setup aws config
		cfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion("us-west-2"))
		if err != nil {
			log.Fatalf("unable to load SDK config, %v", err)
		}
		awstrace.AppendMiddleware(&cfg, awstrace.WithServiceName(os.Getenv("DD_SERVICE")))
		svc = ec2.NewFromConfig(cfg)
	}
}

// kick off the check and slack loops
func main() {
	// Start the tracer and defer the Stop method.
	tracer.Start()
	defer tracer.Stop()
	go startCheckLoop()
	// start slack event loop
	go func() {
		for evt := range client.Events {
			span, ctx := tracer.StartSpanFromContext(context.TODO(), "startSlackLoop")
			switch evt.Type {
			case socketmode.EventTypeConnecting:
				fmt.Println("Connecting to Slack with Socket Mode...")
				span.Finish()
			case socketmode.EventTypeConnectionError:
				fmt.Println("Connection failed. Retrying later...")
				span.Finish()
			case socketmode.EventTypeConnected:
				fmt.Println("Connected to Slack with Socket Mode.")
				span.Finish()
			case socketmode.EventTypeEventsAPI:
				eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					fmt.Printf("Ignored %+v\n", evt)
					span.Finish()
					continue
				}
				// ack event
				client.Ack(*evt.Request)
				switch eventsAPIEvent.Type {
				case slackevents.CallbackEvent:
					innerEvent := eventsAPIEvent.InnerEvent
					switch ev := innerEvent.Data.(type) {
					// check if the event was the opening of app home
					case *slackevents.AppHomeOpenedEvent:
						view, err := AppHomeTabView(ev.User, ctx)
						if err != nil {
							log.Print(err)
							span.Finish()
							continue
						}
						_, err = api.PublishView(ev.User, view, "")
						if err != nil {
							log.Print(err)
							span.Finish()
							continue
						}
						span.Finish()
					}
				default:
					client.Debugf("unsupported Events API event received")
					span.Finish()
				}
			case socketmode.EventTypeInteractive:
				var payload interface{}
				callback, ok := evt.Data.(slack.InteractionCallback)
				if !ok {
					fmt.Printf("Ignored %+v\n", evt)
					span.Finish()
					continue
				}
				switch callback.Type {
				case slack.InteractionTypeBlockActions:
					for _, v := range callback.ActionCallback.BlockActions {
						// send modal if the instance_override button was pressed
						if v.ActionID == "instance_override" {
							client.Ack(*evt.Request)
							user, _, _, err := getUserAndEmail(api, callback.User.ID, ctx)
							if err != nil {
								log.Print(err)
								span.Finish()
								continue
							}
							for _, x := range instanceConfig {
								if x.UserName == user {
									if entry, ok := instanceConfig[user]; ok {
										err = sendModal(api, callback.TriggerID, entry.Override, ctx)
										if err != nil {
											log.Print(err)
											span.Finish()
											continue
										}
									}
								}
							}
							span.Finish()
							continue
						}
					}
					span.Finish()
					continue
				case slack.InteractionTypeShortcut:
					span.Finish()
				case slack.InteractionTypeViewSubmission:
					// add override if user clicked Submit for the override button
					if callback.Type == "view_submission" {
						user, _, _, err := getUserAndEmail(api, callback.User.ID, ctx)
						if err != nil {
							log.Print(err)
							span.Finish()
							continue
						}
						for _, x := range instanceConfig {
							if x.UserName == user {
								if entry, ok := instanceConfig[user]; ok {
									if !entry.Override {
										log.Printf("adding override for:: %v", entry)
										entry.Override = true
										entry.OverrideTime = time.Now()
									} else {
										log.Printf("removing override for:: %v", entry)
										entry.Override = false
									}
									instanceConfig[user] = entry
									err = instanceConfig.writeState()
									if err != nil {
										log.Printf("error writing state: %s", err)
									}
								}
								// update apphome showing the override has changed
								view, err := AppHomeTabView(callback.User.ID, ctx)
								if err != nil {
									log.Print(err)
									span.Finish()
									continue
								}
								client.Ack(*evt.Request, payload)
								// we're going to send this again after a nap to catch the updated instance status status
								_, err = api.PublishView(callback.User.ID, view, "")
								if err != nil {
									log.Print(err)
									span.Finish()
									continue
								}
								// sleep for a bit and then send the updated view
								time.Sleep(checkFrequency + 4*time.Second)
								view, err = AppHomeTabView(callback.User.ID, ctx)
								if err != nil {
									log.Print(err)
									span.Finish()
									continue
								}
								_, err = api.PublishView(callback.User.ID, view, "")
								if err != nil {
									log.Print(err)
									span.Finish()
									continue
								}
							}
						}
						span.Finish()
					}
				default:
					log.Printf("unknown slack event: %v", socketmode.EventTypeInteractive)
					span.Finish()
					continue
				}
				client.Ack(*evt.Request, payload)
				span.Finish()
			case socketmode.EventTypeSlashCommand:
			default:
				span.Finish()
				continue
			}
		}
	}()
	err := client.Run()
	if err != nil {
		log.Print(err)
	}
}

// determines if instance should be on or off at current time
// returns true for on and false for off
func determineNeededState(c instanceDef, ctx context.Context) (on bool, err error) {
	span, _ := tracer.StartSpanFromContext(ctx, "determineNeededState")
	defer span.Finish()
	loc, _ := time.LoadLocation("America/Los_Angeles")

	now := time.Now().In(loc)
	y, m, d := now.Date()
	// get current date
	today, err := time.Parse("2006-January-2 MST", fmt.Sprint(y)+"-"+fmt.Sprint(m)+"-"+fmt.Sprint(d)+" PST")
	if err != nil {
		span.Finish()
		return true, err
	}
	// determine on/off timers for today
	onTimer := today.In(loc).Add(c.OnTimeDuration)
	offTimer := today.In(loc).Add(c.OffTimeDuration)
	// determine if in window for instance to be on
	// if within on window
	if now.After(onTimer) && now.Before(offTimer) {
		if entry, ok := instanceConfig.readState()[c.UserName]; ok {
			writestate := false
			_, _, todayWeekday := time.Now().Date()
			_, _, overrideDay := entry.OverrideTime.Date()
			_, _, WarningIssuedDay := entry.WarningIssuedTime.Date()
			warningTime := offTimer.Add(-10 * time.Minute)
			// if offday and no override, return false
			for _, x := range c.OffDays {
				if today.Weekday().String() == x && !entry.Override {
					// off day and no override, should be off
					return false, nil
				}
			}
			// determine if the user is online within 10 minutes of scheduled shutdown
			if !c.Override && WarningIssuedDay != todayWeekday && !c.WarningIssued && time.Now().After(warningTime) {

				user, err := api.GetUserByEmail(c.Email)
				if err != nil {
					return true, err
				}
				p, err := api.GetUserPresence(user.ID)
				if err != nil {
					return true, err
				}
				// only issue warning if user is active
				if p.Presence == "active" {
					err = issueWarning(user.ID, ctx)
					if err != nil {

						return true, err
					}
					entry.WarningIssued = true
					entry.WarningIssuedTime = time.Now()
					writestate = true
				}
			}
			// clear warning if it wasn't issued today
			if c.WarningIssued && WarningIssuedDay != todayWeekday {
				entry.WarningIssued = false
				writestate = true
			}
			// clear overrides if the overrideDay is not today
			if todayWeekday != overrideDay && c.Override {
				entry.Override = false
				writestate = true
			}
			if writestate {
				instanceConfig[c.UserName] = entry
				err = instanceConfig.writeState()
				if err != nil {
					return true, err
				}
			}
		}
		return true, nil
		// if outside of on window and override is set
	} else if c.Override {
		return true, nil
	} else {
		// instance should be off
		return false, nil
	}
}

// send slack warning message that instance is turning off soon
func issueWarning(userID string, ctx context.Context) error {
	span, _ := tracer.StartSpanFromContext(ctx, "startCheckLoop", tracer.ResourceName("determineNeededState"))
	defer span.Finish()
	message := "Warning, you seem to be online and your remote-dev instance will be shutoff in 10 minutes, click the override button in the home tab to override and save the universe from computers being shut down"
	_, _, err := api.PostMessage(userID, slack.MsgOptionText(message, false))
	if err != nil {
		return err
	}
	return nil
}

// retrieve current status of instance
func getInstanceStatus(username string, ctx context.Context) (string, error) {
	span, _ := tracer.StartSpanFromContext(ctx, "getInstanceStatus")
	defer span.Finish()
	for _, x := range instanceConfig {
		if x.UserName == username {
			name := "tag:Name"
			value := x.Name
			filter := types.Filter{Name: &name, Values: []string{value}}
			input := ec2.DescribeInstancesInput{
				Filters: []types.Filter{filter},
			}
			resp, err := svc.DescribeInstances(context.TODO(), &input)
			if err != nil {
				return "", err
			}
			for _, y := range resp.Reservations {
				for _, z := range y.Instances {
					return string(z.State.Name), nil
				}
			}
		}
	}
	return "", nil
}

// start loop for all instanceconfig checks
func startCheckLoop() {
	log.Printf("Running check loop every %v seconds for instances config:\n%v", checkFrequency, instanceConfig)
	jobTicker := time.NewTicker(checkFrequency)
	defer jobTicker.Stop()
	for {
		span, ctx := tracer.StartSpanFromContext(context.TODO(), "startCheckLoop")
		for _, x := range instanceConfig {
			if strings.Contains(x.Name, "prod") {
				log.Printf("hostname %s contains prod, skipping", x.Name)
				continue
			}
			name := "tag:Name"
			value := x.Name
			filter := types.Filter{Name: &name, Values: []string{value}}
			input := ec2.DescribeInstancesInput{
				Filters: []types.Filter{filter},
			}
			span.SetTag("instanceName", value)
			resp, err := svc.DescribeInstances(context.TODO(), &input)
			if err != nil {
				log.Printf("failed to list instances, %v", err)
				span.Finish()
				continue
			}
			for _, y := range resp.Reservations {
				for _, z := range y.Instances {
					on, err := determineNeededState(x, ctx)
					if err != nil {
						log.Print(err)
						span.Finish()
						continue
					}
					if z.State.Name == "running" && !on {
						for _, i := range z.Tags {
							if *i.Key == "Name" && strings.Contains(x.Name, *i.Value) && !strings.Contains(x.Name, "prod") {
								log.Printf("Instance: %s - %s is on and should be off", *i.Value, *z.InstanceId)
								err := toggleRunning(false, *z.InstanceId, svc, ctx)
								if err != nil {
									log.Print(err)
									span.Finish()
									continue
								}
							}
						}
					} else if z.State.Name != "running" && on {
						for _, i := range z.Tags {
							if *i.Key == "Name" && strings.Contains(x.Name, *i.Value) && !strings.Contains(x.Name, "prod") {
								log.Printf("Instance: %s - %s is off and should be on", *i.Value, *z.InstanceId)
								err := toggleRunning(true, *z.InstanceId, svc, ctx)
								if err != nil {
									log.Print(err)
									span.Finish()
									continue
								}
							}
						}
					} else {
						for _, i := range z.Tags {
							// instance is in correct state, do nothing
							if *i.Key == "Name" && strings.Contains(x.Name, *i.Value) {
							}
						}
					}
				}
			}
		}
		span.Finish()
		<-jobTicker.C
	}
}

// determine state of App Home
func AppHomeTabView(username string, ctx context.Context) (slack.HomeTabViewRequest, error) {
	span, _ := tracer.StartSpanFromContext(ctx, "AppHomeTabView")
	defer span.Finish()
	view := slack.HomeTabViewRequest{}
	ont, offt, offDays, status, override, err := getUserInstanceStatus(username, ctx)
	if err != nil {
		return view, err
	}
	ontString := ont.Format("3:04 pm")
	offString := offt.Format("3:04 pm")
	fun := makeFun()
	uinfo := UserStatus{
		OnTime:   ontString,
		OffTime:  offString,
		OffDays:  offDays,
		Override: override,
		Status:   status,
		ImageUrl: fun,
	}
	// render json block templates
	t1, err := template.ParseFS(appHomeAssets, "appHomeViewsAssets/AppHomeView.json")
	if err != nil {
		return view, err
	}
	var tpl1 bytes.Buffer
	err = t1.Execute(&tpl1, uinfo)
	if err != nil {
		return view, err
	}
	str1, _ := ioutil.ReadAll(&tpl1)
	json.Unmarshal(str1, &view)
	// user info
	t, err := template.ParseFS(appHomeAssets, "appHomeViewsAssets/UserBlock.json")
	if err != nil {
		return view, err
	}
	var tpl bytes.Buffer
	err = t.Execute(&tpl, uinfo)
	if err != nil {
		return view, err
	}
	str, _ := ioutil.ReadAll(&tpl)
	user_view := slack.HomeTabViewRequest{}
	json.Unmarshal(str, &user_view)
	view.Blocks.BlockSet = append(view.Blocks.BlockSet, user_view.Blocks.BlockSet...)
	return view, nil
}

// get the desired instance status
func getUserInstanceStatus(user string, ctx context.Context) (onTime time.Time, offTime time.Time, offDays []string, instanceStatus string, override bool, err error) {
	span, _ := tracer.StartSpanFromContext(ctx, "getUserInstanceStatus")
	defer span.Finish()
	userinfo, err := api.GetUserInfo(user)
	if err != nil {
		return onTime, offTime, offDays, instanceStatus, override, err
	}
	if entry, ok := instanceConfig[userinfo.Name]; ok {
		loc, _ := time.LoadLocation("America/Los_Angeles")
		now := time.Now().In(loc)
		y, m, d := now.Date()
		today, err := time.Parse("2006-January-2 MST", fmt.Sprint(y)+"-"+fmt.Sprint(m)+"-"+fmt.Sprint(d)+" PST")
		if err != nil {
			return onTime, offTime, offDays, instanceStatus, override, err
		}
		onTime = today.In(loc).Add(entry.OnTimeDuration)
		offTime = today.In(loc).Add(entry.OffTimeDuration)
		instanceStatus, err = getInstanceStatus(userinfo.Name, ctx)
		if err != nil {
			return onTime, offTime, offDays, instanceStatus, override, err
		}
		override = instanceConfig.readState()[userinfo.Name].Override
		offDays = instanceConfig.readState()[userinfo.Name].OffDays
		return onTime, offTime, offDays, instanceStatus, override, nil
	}
	return onTime, offTime, offDays, instanceStatus, override, fmt.Errorf("user not found")
}

// get user, email and presence info from slack
func getUserAndEmail(api *slack.Client, userID string, ctx context.Context) (user string, email string, presence string, err error) {
	span, _ := tracer.StartSpanFromContext(ctx, "getUserandEmail")
	defer span.Finish()
	uinfo, err := api.GetUserInfo(userID)
	if err != nil {
		return user, email, presence, err
	}
	p, err := api.GetUserPresence(userID)
	if err != nil {
		return user, email, presence, err
	}
	email = uinfo.Profile.Email
	user = uinfo.Name
	presence = p.Presence
	return user, email, presence, nil
}

// send modal to user asking if they want to override
func sendModal(client *slack.Client, triggerID string, currentOverrideStatus bool, ctx context.Context) error {
	span, _ := tracer.StartSpanFromContext(ctx, "sendModal")
	defer span.Finish()
	titleText := slack.NewTextBlockObject(slack.PlainTextType, "instance-scheduler", false, false)
	closeText := slack.NewTextBlockObject(slack.PlainTextType, "No", false, false)
	submitText := slack.NewTextBlockObject(slack.PlainTextType, "Yes", false, false)
	contextText := slack.NewTextBlockObject(slack.MarkdownType, "Would you like to prevent your ec2 instance from automatically turning off tonight?", false, false)
	if currentOverrideStatus {
		contextText = slack.NewTextBlockObject(slack.MarkdownType, "Would you like to remove the override for your ec2 instance to it turns off at the scheduled time?", false, false)
	}
	contextBlock := slack.NewContextBlock("context", contextText)
	instanceOverridePlaceholder := slack.NewTextBlockObject(slack.PlainTextType, "instance override", false, false)
	instanceOverrideElement := slack.NewPlainTextInputBlockElement(instanceOverridePlaceholder, "instance_override")
	instanceOverrideElement.MaxLength = 80
	blocks := slack.Blocks{
		BlockSet: []slack.Block{
			contextBlock,
			// instanceOverrideBlock,
		},
	}
	var modalRequest slack.ModalViewRequest
	modalRequest.Type = slack.ViewType("modal")
	modalRequest.Title = titleText
	modalRequest.Close = closeText
	modalRequest.Submit = submitText
	modalRequest.Blocks = blocks
	modalRequest.CallbackID = "instance_override"
	_, err := client.OpenView(triggerID, modalRequest)
	if err != nil {
		fmt.Printf("Error opening view: %s", err)
	}
	return nil
}

// pass true to turn on, pass false to turn off, id is instance-id
func toggleRunning(on bool, id string, client *ec2.Client, ctx context.Context) error {
	span, _ := tracer.StartSpanFromContext(ctx, "toggleRunning")
	defer span.Finish()
	ids := []string{id}
	if !on {
		ops := ec2.StopInstancesInput{
			InstanceIds: ids,
		}
		_, err := client.StopInstances(context.TODO(), &ops)
		if err != nil {
			return err
		}
		log.Printf("sent stop request for instance %s", id)
	} else {
		ops := ec2.StartInstancesInput{
			InstanceIds: ids,
		}
		_, err := client.StartInstances(context.TODO(), &ops)
		if err != nil {
			return err
		}
		log.Printf("sent start request for instance %s", id)
	}
	return nil
}
