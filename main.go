package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

var lastCheck string

var status = map[string]string{
	"mysql-app":             "ok",
	"mysql-pbx":             "ok",
	"rabbitmq":              "ok",
	"redis":                 "ok",
	"elasticsearch":         "ok",
	"elasticsearch-logging": "ok",
}

func checkMysql(host string, user string, pass string) error {
	log.Printf("[MYSQL]: Checking mysql host: %s", host)
	out, err := exec.Command("mysqladmin", "-h", host, "-u", user, fmt.Sprintf("-p%s", pass), "status").Output()

	if err != nil {
		log.Print(err)
		return err
	}

	if !strings.Contains(string(out), "Uptime") {
		return errors.New("[MYSQL]: Output does not match")
	}

	return nil
}

func checkRedis(host string, pass string, port string) error {
	log.Printf("[REDIS]: Checking redis host: %s", host)
	cmd := []string{"-h", host, "-p", port}

	if len(pass) > 0 {
		cmd = append(cmd, "-a", pass)
	}

	cmd = append(cmd, "PING")

	out, err := exec.Command("redis-cli", cmd...).Output()

	if err != nil {
		return err
	}

	if !strings.Contains(string(out), "PONG") {
		return errors.New("[REDIS]: Output does not match")
	}

	return nil
}

func checkElasticsearch(baseUrl string) error {
	resp, err := http.Get(fmt.Sprintf("%s/_cluster/health", baseUrl))

	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		log.Printf("[ELASTICSEARCH]: Checking endpoint: %s", baseUrl)
		bodyBytes, err := ioutil.ReadAll(resp.Body)

		if err != nil {
			return err
		}

		var bodyJSON map[string]interface{}

		if err := json.Unmarshal(bodyBytes, &bodyJSON); err != nil {
			return err
		}

		if bodyJSON["status"] != "green" {
			return errors.New("[ELASTICSEARCH]: Health concern")
		}

		return nil
	}

	return errors.New("[ELASTICSEARCH]: Endpoint returns status other then OK")
}

func authGet(url string, username string, password string) (*http.Response, error) {
	client := &http.Client{}
	req, err := http.NewRequest("GET", url, nil)

	if err != nil {
		return nil, err
	}

	req.SetBasicAuth(username, password)

	return client.Do(req)
}

func checkRabbitmq(baseUrl string, user string, password string) error {
	resp, err := authGet(fmt.Sprintf("%s/api/nodes", baseUrl), user, password)

	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		log.Printf("[RABBITMQ]: Obtaining cluster info for: %s", baseUrl)

		bodyBytes, err := ioutil.ReadAll(resp.Body)

		if err != nil {
			return err
		}

		var bodyJSON []map[string]interface{}

		if err := json.Unmarshal(bodyBytes, &bodyJSON); err != nil {
			return err
		}

		for _, item := range bodyJSON {
			var nodeName = item["name"]

			resp, err := authGet(fmt.Sprintf("%s/api/healthchecks/node/%s", baseUrl, nodeName), user, password)
			log.Printf("[RABBITMQ]: Checking health of %s", nodeName)

			if err != nil {
				return err
			}

			defer resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				nodeBytes, err := ioutil.ReadAll(resp.Body)

				if err != nil {
					return err
				}

				var nodeJSON = map[string]interface{}{}

				if err := json.Unmarshal(nodeBytes, &nodeJSON); err != nil {
					return err
				}

				if nodeJSON["status"] != "ok" {
					return errors.New("[RABBITMQ]: Health check")
				}

				continue
			}

			return errors.New("[RABBITMQ]: Node endpoint returns status other then OK")
		}

		return nil
	}

	return errors.New("[RABBITMQ]: Endpoint returns status other then OK")
}

func checkState(interval time.Duration) {
	if err := checkMysql(os.Getenv("MYSQL_APP_HOST"),
		os.Getenv("MYSQL_APP_USER"),
		os.Getenv("MYSQL_APP_PASS"),
	); err != nil {
		status["mysql-app"] = "down"
	} else {
		status["mysql-app"] = "ok"
	}

	if err := checkMysql(os.Getenv("MYSQL_PBX_HOST"),
		os.Getenv("MYSQL_PBX_USER"),
		os.Getenv("MYSQL_PBX_PASS"),
	); err != nil {
		status["mysql-pbx"] = "down"
	} else {
		status["mysql-pbx"] = "ok"
	}

	if err := checkRedis(os.Getenv("REDIS_HOST"),
		os.Getenv("REDIS_PASS"),
		"6379",
	); err != nil {
		status["redis"] = "down"
	} else {
		status["redis"] = "ok"
	}

	if err := checkElasticsearch(os.Getenv("ELASTICSEARCH_URL")); err != nil {
		status["elasticsearch"] = "down"
	} else {
		status["elasticsearch"] = "ok"
	}

	if err := checkElasticsearch(os.Getenv("ELASTICSEARCH_LOGGING_URL")); err != nil {
		status["elasticsearch-logging"] = "down"
	} else {
		status["elasticsearch-logging"] = "ok"
	}

	if err := checkRabbitmq(os.Getenv("RABBITMQ_URL"),
		os.Getenv("RABBITMQ_USER"),
		os.Getenv("RABBITMQ_PASS"),
	); err != nil {
		status["rabbitmq"] = "down"
	} else {
		status["rabbitmq"] = "ok"
	}

	loc, _ := time.LoadLocation("Europe/Amsterdam")
	lastCheck = time.Now().In(loc).String()

	time.Sleep(interval * time.Second)

	checkState(interval)
}

func main() {
	err := godotenv.Load()

	if err != nil {
		log.Print("No .env file found")
	}

	serve := flag.String("serve", ":9000", "Serve")
	interval := flag.Duration("interval", 30, "Interval for checks")

	flag.Parse()

	go checkState(*interval)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		for _, item := range status {
			if item == "down" {
				w.WriteHeader(http.StatusNotFound)
			}
		}

		encode, _ := json.MarshalIndent(map[string]interface{}{
			"checks":     status,
			"last_check": lastCheck,
		}, "", "    ")

		w.Header().Set("Content-Type", "application/json")
		w.Write(encode)
	})

	if err := http.ListenAndServe(*serve, nil); err != nil {
		log.Fatal(err)
	}
}
