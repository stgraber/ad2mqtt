package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"

	"github.com/stgraber/ad2mqtt/decoder"

	"github.com/eclipse/paho.mqtt.golang"
	"github.com/jacobsa/go-serial/serial"
)

type zone struct {
	Name         string `json:"name"`
	FriendlyName string `json:"friendly_name"`
	Type         string `json:"type"`
	Disabled     bool   `json:"disabled"`
}

func main() {
	log.SetOutput(os.Stdout)

	err := run()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Load the zones.
	content, err := ioutil.ReadFile(os.Getenv("CONFIG"))
	if err != nil {
		return fmt.Errorf("Failed to load the config: %w", err)
	}

	var zones map[string]zone
	err = json.Unmarshal(content, &zones)
	if err != nil {
		return fmt.Errorf("Failed to unmarshal the config: %w", err)
	}

	// Setup serial connection.
	options := serial.OpenOptions{
		PortName:        os.Getenv("AD_PATH"),
		BaudRate:        115200,
		DataBits:        8,
		StopBits:        1,
		MinimumReadSize: 4,
	}

	port, err := serial.Open(options)
	if err != nil {
		return fmt.Errorf("Failed to open serial port: %w", err)
	}
	defer port.Close()

	// Setup alarm decoder.
	ad := alarmdecoder.New(port)

	// Setup MQTT connection.
	mqttOpts := mqtt.NewClientOptions()
	mqttOpts.AddBroker(os.Getenv("MQTT_HOST"))
	mqttOpts.SetClientID("ad2mqtt")
	mqttOpts.SetUsername(os.Getenv("MQTT_USERNAME"))
	mqttOpts.SetPassword(os.Getenv("MQTT_PASSWORD"))
	mqttOpts.SetAutoReconnect(true)
	mqttClient := mqtt.NewClient(mqttOpts)
	if token := mqttClient.Connect(); token.Wait() && token.Error() != nil {
		return token.Error()
	}

	// Setup MQTT topics.
	data := `{
    "code": "REMOTE_CODE",
    "code_arm_required": false,
    "code_disarm_required": true,
    "code_trigger_required": false,
    "command_template": "{\"action\": \"{{ action }}\", \"code\": \"{{ code }}\"}",
    "command_topic": "homeassistant/alarm_control_panel/ad2mqtt/command",
    "name": "ad2mqtt",
    "state_topic": "homeassistant/alarm_control_panel/ad2mqtt/state"
}`
	if token := mqttClient.Publish("homeassistant/alarm_control_panel/ad2mqtt/config", 0, true, data); token.Wait() && token.Error() != nil {
		return token.Error()
	}

	mqttCallback := func(client mqtt.Client, msg mqtt.Message) {
		value := msg.Payload()

		type mqttAction struct {
			Action string `json:"action"`
			Code   string `json:"code"`
		}

		var action mqttAction
		err := json.Unmarshal(value, &action)
		if err != nil {
			log.Printf("[mqtt] Failed to parse action: %v", err)
		}

		if action.Action == "ARM_HOME" {
			ad.Write([]byte("#"))
			ad.Write([]byte("3"))

			log.Printf("[mqtt] Armed (home)")
		} else if action.Action == "ARM_AWAY" {
			ad.Write([]byte("#"))
			ad.Write([]byte("2"))

			log.Printf("[mqtt] Armed (away)")
		} else if action.Action == "DISARM" {
			if action.Code == "None" {
				log.Printf("[mqtt] Failed to disarm: No code provided")
				return
			}

			for _, c := range action.Code {
				ad.Write([]byte(string(c)))
			}
			ad.Write([]byte("1"))

			log.Printf("[mqtt] Disarmed")
		}
	}

	if token := mqttClient.Subscribe("homeassistant/alarm_control_panel/ad2mqtt/command", 0, mqttCallback); token.Wait() && token.Error() != nil {
		return token.Error()
	}

	for _, zone := range zones {
		if zone.Disabled || zone.Name == "" {
			continue
		}

		// Define the sensor.
		data := fmt.Sprintf(`{
    "unique_id": "%s",
    "name": "%s",
    "state_topic": "homeassistant/binary_sensor/%s/state",
    "payload_on": "on",
    "payload_off": "off",
    "device_class": "%s"
}`, zone.Name, zone.FriendlyName, zone.Name, zone.Type)
		if token := mqttClient.Publish(fmt.Sprintf("homeassistant/binary_sensor/%s/config", zone.Name), 0, true, data); token.Wait() && token.Error() != nil {
			return token.Error()
		}

		if token := mqttClient.Publish(fmt.Sprintf("homeassistant/binary_sensor/%s/state", zone.Name), 0, true, "off"); token.Wait() && token.Error() != nil {
			return token.Error()
		}

	}

	// Process incoming message.
	alarmState := ""
	zoneState := map[string]bool{}
	for {
		msg, err := ad.Read()
		if err != nil {
			log.Printf("[alarm] Unknown message from alarm: %v", err)
			continue
		}

		// Update the alarm state.
		var newAlarmState string
		if msg.AlarmSounding || msg.AlarmHasOccured {
			newAlarmState = "triggered"
		} else if msg.ArmedHome {
			newAlarmState = "armed_home"
		} else if msg.ArmedAway {
			newAlarmState = "armed_away"
		} else if !msg.Ready {
			newAlarmState = "pending"
		} else {
			newAlarmState = "disarmed"
		}

		if newAlarmState != alarmState {
			alarmState = newAlarmState
			if token := mqttClient.Publish("homeassistant/alarm_control_panel/ad2mqtt/state", 0, true, alarmState); token.Wait() && token.Error() != nil {
				return token.Error()
			}

			log.Printf("[alarm] Set state to %s", alarmState)
		}

		// Handle zone triggers.
		zone, ok := zones[msg.Zone]
		if !ok {
			log.Printf("[alarm] Unknown zone %q has been triggered", msg.Zone)
		}

		if !zone.Disabled {
			state := zoneState[zone.Name]
			if !state {
				if token := mqttClient.Publish(fmt.Sprintf("homeassistant/binary_sensor/%s/state", zone.Name), 0, true, "on"); token.Wait() && token.Error() != nil {
					return token.Error()
				}

				log.Printf("[alarm] Zone %q has been triggered", zone.Name)
				zoneState[msg.Zone] = true
			}
		}

		// Once things are quiet, look for any formerly triggered zone.
		if msg.Ready {
			for k, v := range zoneState {
				if k == msg.Zone {
					continue
				}

				if !v {
					continue
				}

				zoneState[k] = false
				zone := zones[k]
				if zone.Name != "" {
					if token := mqttClient.Publish(fmt.Sprintf("homeassistant/binary_sensor/%s/state", zone.Name), 0, true, "off"); token.Wait() && token.Error() != nil {
						return token.Error()
					}

					log.Printf("[alarm] Zone %q has been cleared", zone.Name)
				}
			}
		}
	}

	return nil
}
