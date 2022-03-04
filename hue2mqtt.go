package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/amimof/huego"
	"github.com/donovanhide/eventsource"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"golang.org/x/net/trace"
)

var (
	listenAddress = flag.String("listen",
		":8779",
		"listen address for HTTP API (e.g. for Shelly buttons)")

	mqttBroker = flag.String("mqtt_broker",
		"tcp://dr.lan:1883",
		"MQTT broker address for github.com/eclipse/paho.mqtt.golang")

	mqttPrefix = flag.String("mqtt_topic",
		"github.com/stapelberg/hue2mqtt/",
		"MQTT topic prefix")

	hueKey = flag.String("hue_key",
		os.Getenv("HUE_KEY"),
		"secret key for communicating with your local Philips Hue bridge, see https://developers.meethue.com/develop/get-started-2/ for how to generate one")
)

type lightController struct {
	bridge *huego.Bridge
	on     map[string]bool
}

func (lc *lightController) visualbell(lightId int, desired huego.State) error {
	l, err := lc.bridge.GetLight(lightId)
	if err != nil {
		return err
	}
	old := huego.State{
		Hue: l.State.Hue,
		On:  l.State.On,
	}
	log.Printf("light %d, set visualbell=%+v", lightId, desired)
	lc.bridge.SetLightState(lightId, desired)
	go func() {
		time.Sleep(2 * time.Second)
		log.Printf("light %d, revert to old=%+v", lightId, old)
		lc.bridge.SetLightState(lightId, old)
	}()
	return nil
}

func (lc *lightController) command(room, command string, desired huego.State) error {
	// TODO: map room name to group ids
	groupId := 1
	if room == "bedroom" {
		groupId = 2
	} else if room == "living" {
		groupId = 3
	} else if room == "kitchen" {
		groupId = 4
	}

	switch command {
	case "on":
		desired.On = true

	case "off":
		desired.On = false

	case "toggle":
		desired.On = !lc.on[fmt.Sprintf("/groups/%d", groupId)]

	case "visualbell":
		desired.On = true
		lightId := 5
		switch room {
		case "living":
			lightId = 5
		case "kitchen":
			lightId = 7
		default:
			return nil
		}
		return lc.visualbell(lightId, desired)

	default:
		log.Printf("unknown command: %q", command)
		return fmt.Errorf("unknown command: %q", command)
	}

	log.Printf("room %q, set on=%v, bri=%v", room, desired.On, desired.Bri)
	lc.bridge.SetGroupStateContext(context.Background(), groupId, desired)

	return nil
}

func (lc *lightController) eventMessageHandler(_ mqtt.Client, m mqtt.Message) {
	log.Printf("mqtt event: %s: %v", m.Topic(), string(m.Payload()))

	var groupState struct {
		ID   string `json:"id"`
		IDv1 string `json:"id_v1"`
		On   struct {
			On bool `json:"on"`
		} `json:"on"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(m.Payload(), &groupState); err != nil {
		log.Print(err)
		return
	}
	lc.on[groupState.IDv1] = groupState.On.On

	log.Printf("lc.on = %+v", lc.on)
}

func (lc *lightController) commandMessageHandler(_ mqtt.Client, m mqtt.Message) {
	log.Printf("mqtt: %s: %q", m.Topic(), string(m.Payload()))
	parts := strings.Split(strings.TrimPrefix(m.Topic(), *mqttPrefix+"cmd/light/"), "/")
	if len(parts) != 2 {
		log.Printf("parts = %q", parts)
		return
	}
	room := parts[0]
	command := parts[1]

	var desired huego.State
	if err := json.Unmarshal(m.Payload(), &desired); err != nil {
		log.Printf("error unmarshaling payload: %v", err)
	}
	if err := lc.command(room, command, desired); err != nil {
		log.Print(err)
		return
	}
}

func publish(mqttClient mqtt.Client, ev eventsource.Event) error {
	var events []struct {
		Id           string            `json:"id"`
		CreationTime time.Time         `json:"creationtime"`
		Type         string            `json:"type"`
		Data         []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(ev.Data()), &events); err != nil {
		return err
	}
	for _, ev := range events {
		for _, d := range ev.Data {
			var evId struct {
				IDv1 string `json:"id_v1"`
			}
			if err := json.Unmarshal(d, &evId); err != nil {
				log.Print(err)
				continue
			}

			evId.IDv1 = strings.TrimPrefix(evId.IDv1, "/")
			mqttClient.Publish(
				*mqttPrefix+evId.IDv1,
				0,    /* qos */
				true, /* retained */
				string(d))
		}
	}
	return nil
}

func subscribe(mqttClient mqtt.Client, topic string, hdl mqtt.MessageHandler) error {
	const qosAtMostOnce = 0
	log.Printf("Subscribing to %s", topic)
	token := mqttClient.Subscribe(topic, qosAtMostOnce, hdl)
	token.Wait()
	if err := token.Error(); err != nil {
		return fmt.Errorf("subscription failed: %v", err)
	}
	return nil
}

func hue2mqtt() error {
	// TODO: can we do extremely long keep-alive for an already-established connection?
	log.Printf("discovering bridge")
	bridge, err := huego.Discover()
	if err != nil {
		return err
	}
	log.Printf("discovered: %s", bridge.Host)
	bridge.User = *hueKey
	lc := &lightController{
		bridge: bridge,
		on:     make(map[string]bool),
	}

	opts := mqtt.NewClientOptions().AddBroker(*mqttBroker)
	clientID := "https://github.com/stapelberg/hue2mqtt"
	if hostname, err := os.Hostname(); err == nil {
		clientID += "@" + hostname
	}
	opts.SetClientID(clientID)
	opts.SetConnectRetry(true)
	opts.OnConnect = func(c mqtt.Client) {
		if err := subscribe(c, *mqttPrefix+"cmd/light/#", lc.commandMessageHandler); err != nil {
			log.Print(err)
		}

		if err := subscribe(c, *mqttPrefix+"groups/+", lc.eventMessageHandler); err != nil {
			log.Print(err)
		}
	}
	mqttClient := mqtt.NewClient(opts)
	if token := mqttClient.Connect(); token.Wait() && token.Error() != nil {
		return fmt.Errorf("MQTT connection failed: %v", token.Error())
	}

	trace.AuthRequest = func(req *http.Request) (any, sensitive bool) { return true, true }

	// Read events via the Hue API v2 eventsource protocol
	req, err := http.NewRequest("GET", "https://"+bridge.Host+"/eventstream/clip/v2", nil)
	if err != nil {
		return err
	}
	req.Header.Set("hue-application-key", *hueKey)
	// TODO: implement TLS verification
	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	stream, err := eventsource.SubscribeWith("", http.DefaultClient, req)
	if err != nil {
		return fmt.Errorf("Hue API v2 request failed: %v", err)
	}
	go func() {
		defer stream.Close()
		for {
			select {
			case ev := <-stream.Events:
				if err := publish(mqttClient, ev); err != nil {
					log.Print(err)
					continue
				}
			case err := <-stream.Errors:
				log.Printf("log streaming error: %v", err)
			}
		}
	}()

	{
		// For debugging:
		lights, err := bridge.GetLights()
		if err != nil {
			return err
		}
		for _, l := range lights {
			log.Printf("light: %+v", l)
		}

		groups, err := bridge.GetGroups()
		if err != nil {
			return err
		}
		for _, l := range groups {
			log.Printf("group: %+v", l)
			log.Printf("state: %+v", l.GroupState)
			lc.on[fmt.Sprintf("/groups/%d", l.ID)] = l.GroupState.AllOn
		}
	}

	log.Printf("lc.on = %+v", lc.on)

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/requests/", trace.Traces)
	mux.HandleFunc("/light/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("http: %s", r.URL.Path)
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/light/"), "/")
		if len(parts) != 2 {
			log.Printf("parts = %q", parts)
			return
		}
		room := parts[0]
		command := parts[1]

		var desired huego.State
		if brightness := r.FormValue("brightness"); brightness != "" {
			b, err := strconv.ParseInt(brightness, 0, 64)
			if err != nil {
				log.Print(err)
				return
			}
			// b is a percentage, i.e. [0, 100].
			// bri is an uint8, i.e. [0, 254].
			desired.Bri = uint8(254 * (float64(b) / 100))
		}
		if hue := r.FormValue("hue"); hue != "" {
			b, err := strconv.ParseInt(hue, 0, 64)
			if err != nil {
				log.Print(err)
				return
			}
			desired.Hue = uint16(b)
		}
		if err := lc.command(room, command, desired); err != nil {
			log.Print(err)
			return
		}
	})

	log.Printf("http.ListenAndServe(%q)", *listenAddress)
	if err := http.ListenAndServe(*listenAddress, mux); err != nil {
		return err
	}
	return nil
}

func main() {
	flag.Parse()
	if err := hue2mqtt(); err != nil {
		log.Fatal(err)
	}
}
