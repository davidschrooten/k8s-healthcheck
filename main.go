package main

import (
	"crypto/tls"
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

var (
	lastCheck       string
	statusResources = map[string]string{
		"mysql-app":             "ok",
		"mysql-pbx":             "ok",
		"rabbitmq":              "ok",
		"redis":                 "ok",
		"elasticsearch":         "ok",
		"elasticsearch-logging": "ok",
	}
	kubernetes *Kubernetes
	serve      = flag.String("serve", ":9000", "Serve")
	interval   = flag.Duration("interval", 30, "Interval for checks")
)

type Kubernetes struct {
	Deployment  []map[string]interface{}
	StatefulSet []map[string]interface{}
	Job         []map[string]interface{}
	CronJob     []map[string]interface{}
}

func searchNameStruct(stateChild []map[string]interface{}, name string) int {
	for key, item := range stateChild {
		if item["name"] == name {
			return key
		}
	}

	return -1
}

func checkChild(stateChild []map[string]interface{}) []map[string]interface{} {
	for key, item := range stateChild {
		var length = len(item["pods"].(map[string]string))

		for _, v := range item["pods"].(map[string]string) {
			if v != "ok" {
				length--
			}
		}

		if length == 0 {
			for itemName, v := range item {
				if v == "ok" {
					stateChild[key][itemName] = "fail"
				}
			}
		}
	}

	return stateChild
}

func getJobParent(host string, token string, name string) (string, error) {
	resp, err := jwtGet(fmt.Sprintf("%s/apis/batch/v1/namespaces/qooqie-production/jobs?fieldSelector=metadata.name=%s", host, name), token)

	if err != nil {
		return "", err
	}

	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		bodyBytes, err := ioutil.ReadAll(resp.Body)

		if err != nil {
			return "", err
		}

		var bodyJSON map[string]interface{}

		if err := json.Unmarshal(bodyBytes, &bodyJSON); err != nil {
			return "", err
		}

		var items = bodyJSON["items"].([]interface{})

		if len(items) == 0 {
			return "", errors.New("[KUBERNETES]: Cannot find job")
		}

		ownerReferences, hasOwnerReferences := items[0].(map[string]interface{})["metadata"].(map[string]interface{})["ownerReferences"]

		var parentName string

		if hasOwnerReferences {
			parentName = ownerReferences.([]interface{})[0].(map[string]interface{})["name"].(string)
		}

		return parentName, nil
	}

	return "", errors.New("[KUBERNETES]: Job endpoint returns status other then ok")
}

func getRSParent(host string, token string, name string) (string, error) {
	resp, err := jwtGet(fmt.Sprintf("%s/apis/apps/v1/namespaces/qooqie-production/replicasets?fieldSelector=metadata.name=%s", host, name), token)

	if err != nil {
		return "", err
	}

	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		bodyBytes, err := ioutil.ReadAll(resp.Body)

		if err != nil {
			return "", err
		}

		var bodyJSON map[string]interface{}

		if err := json.Unmarshal(bodyBytes, &bodyJSON); err != nil {
			return "", err
		}

		var items = bodyJSON["items"].([]interface{})

		if len(items) == 0 {
			return "", errors.New("[KUBERNETES]: Cannot find replicaset")
		}

		var parentName = items[0].(map[string]interface{})["metadata"].(map[string]interface{})["ownerReferences"].([]interface{})[0].(map[string]interface{})["name"].(string)

		return parentName, nil
	}

	return "", errors.New("[KUBERNETES]: Replicaset endpoint returns status other then ok")
}

func checkKubernetes(host string, token string) (*Kubernetes, error) {
	log.Printf("[KUBERNETES]: Checking kubernetes cluster: %s", host)
	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	resp, err := jwtGet(fmt.Sprintf("%s/api/v1/namespaces/qooqie-production/pods", host), token)

	if err != nil {
		return &Kubernetes{}, err
	}

	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		bodyBytes, err := ioutil.ReadAll(resp.Body)

		if err != nil {
			return &Kubernetes{}, err
		}

		var bodyJSON map[string]interface{}

		if err := json.Unmarshal(bodyBytes, &bodyJSON); err != nil {
			return &Kubernetes{}, err
		}

		var state = &Kubernetes{}

		var items = bodyJSON["items"].([]interface{})

		for _, item := range items {
			castItem := item.(map[string]interface{})

			var (
				name       = castItem["metadata"].(map[string]interface{})["name"].(string)
				parentName = castItem["metadata"].(map[string]interface{})["ownerReferences"].([]interface{})[0].(map[string]interface{})["name"].(string)
				parentType = castItem["metadata"].(map[string]interface{})["ownerReferences"].([]interface{})[0].(map[string]interface{})["kind"].(string)
				statuses   = castItem["status"].(map[string]interface{})["containerStatuses"].([]interface{})
				podStatus  = "ok"
			)

			for _, status := range statuses {
				castStatus := status.(map[string]interface{})
				_, hasRunning := castStatus["state"].(map[string]interface{})

				if !castStatus["ready"].(bool) && !hasRunning {
					podStatus = "down"
				}

				lastState, hasLastState := castStatus["lastState"].(map[string]interface{})

				if hasLastState {
					terminated, hasTerminated := lastState["terminated"]

					if hasTerminated {
						if terminated.(map[string]interface{})["reason"].(string) != "Completed" {
							podStatus = "error"
						}
					}
				}
			}

			switch parentType {
			case "ReplicaSet":
				rsName, err := getRSParent(host, token, parentName)

				if err != nil {
					return &Kubernetes{}, err
				}

				index := searchNameStruct(state.Deployment, rsName)

				if index != -1 {
					current := state.Deployment[index]

					pods := current["pods"].(map[string]string)
					pods[name] = "ok"

					current["pods"] = pods

					state.Deployment[index] = current
				} else {
					state.Deployment = append(state.Deployment, map[string]interface{}{
						rsName: "ok",
						"pods": map[string]string{
							name: podStatus,
						},
					})
				}
			case "Job":
				jobName, err := getJobParent(host, token, parentName)

				if err != nil {
					return &Kubernetes{}, err
				}

				// Check if cronjob or not
				if len(jobName) > 0 {
					if index := searchNameStruct(state.CronJob, jobName); index != -1 {
						current := state.CronJob[index]

						pods := current["pods"].(map[string]string)
						pods[name] = podStatus

						current["pods"] = pods

						state.CronJob[index] = current
					} else {
						state.CronJob = append(state.CronJob, map[string]interface{}{
							jobName: "ok",
							"pods": map[string]string{
								name: podStatus,
							},
						})
					}
				} else {
					if index := searchNameStruct(state.Job, jobName); index != -1 {
						current := state.Job[index]

						pods := current["pods"].(map[string]string)
						pods[name] = podStatus

						current["pods"] = pods

						state.Job[index] = current
					} else {
						state.Job = append(state.Job, map[string]interface{}{
							name: "ok",
							"pods": map[string]string{
								name: podStatus,
							},
						})
					}
				}
			case "StatefulSet":
				index := searchNameStruct(state.StatefulSet, parentName)

				if index != -1 {
					current := state.StatefulSet[index]

					pods := current["pods"].(map[string]string)
					pods[name] = podStatus

					current["pods"] = pods

					state.StatefulSet[index] = current
				} else {
					state.StatefulSet = append(state.StatefulSet, map[string]interface{}{
						parentName: "ok",
						"pods": map[string]string{
							name: podStatus,
						},
					})
				}
			}
		}

		// Check parent state
		state.Deployment = checkChild(state.Deployment)
		state.StatefulSet = checkChild(state.StatefulSet)
		state.CronJob = checkChild(state.CronJob)
		state.Job = checkChild(state.Job)

		return state, nil
	}

	return &Kubernetes{}, errors.New("[KUBERNETES]: Pod returns status other then ok")
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

func jwtGet(url string, token string) (*http.Response, error) {
	client := &http.Client{}
	req, err := http.NewRequest("GET", url, nil)

	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

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
		statusResources["mysql-app"] = "down"
	} else {
		statusResources["mysql-app"] = "ok"
	}

	if err := checkMysql(os.Getenv("MYSQL_PBX_HOST"),
		os.Getenv("MYSQL_PBX_USER"),
		os.Getenv("MYSQL_PBX_PASS"),
	); err != nil {
		statusResources["mysql-pbx"] = "down"
	} else {
		statusResources["mysql-pbx"] = "ok"
	}

	if err := checkRedis(os.Getenv("REDIS_HOST"),
		os.Getenv("REDIS_PASS"),
		"6379",
	); err != nil {
		statusResources["redis"] = "down"
	} else {
		statusResources["redis"] = "ok"
	}

	if err := checkElasticsearch(os.Getenv("ELASTICSEARCH_URL")); err != nil {
		statusResources["elasticsearch"] = "down"
	} else {
		statusResources["elasticsearch"] = "ok"
	}

	if err := checkElasticsearch(os.Getenv("ELASTICSEARCH_LOGGING_URL")); err != nil {
		statusResources["elasticsearch-logging"] = "down"
	} else {
		statusResources["elasticsearch-logging"] = "ok"
	}

	if err := checkRabbitmq(os.Getenv("RABBITMQ_URL"),
		os.Getenv("RABBITMQ_USER"),
		os.Getenv("RABBITMQ_PASS"),
	); err != nil {
		statusResources["rabbitmq"] = "down"
	} else {
		statusResources["rabbitmq"] = "ok"
	}

	if state, err := checkKubernetes(os.Getenv("KUBERNETES_HOST"),
		os.Getenv("KUBERNETES_TOKEN"),
	); err != nil {
		kubernetes = &Kubernetes{}
	} else {
		kubernetes = state
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

	flag.Parse()

	go checkState(*interval)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		for _, item := range statusResources {
			if item == "down" {
				w.WriteHeader(http.StatusNotFound)
				break
			}
		}

		encode, _ := json.MarshalIndent(map[string]interface{}{
			"resources":  statusResources,
			"kubernetes": kubernetes,
			"last_check": lastCheck,
		}, "", "    ")

		w.Header().Set("Content-Type", "application/json")
		w.Write(encode)
	})

	if err := http.ListenAndServe(*serve, nil); err != nil {
		log.Fatal(err)
	}
}
