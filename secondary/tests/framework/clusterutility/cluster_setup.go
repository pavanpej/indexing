package clusterutility

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	couchbase "github.com/couchbase/indexing/secondary/dcp"
)

var ErrRebalanceTimedout = errors.New("Rebalance did not finish after 30 minutes")
var ErrRebalanceFailed = errors.New("Rebalance failed")

func getInitServicesUrl(serverAddr string) string {
	return prependHttp(serverAddr) + "/node/controller/setupServices"
}

func getWebCredsUrl(serverAddr string) string {
	return prependHttp(serverAddr) + "/settings/web"
}

func getQuotaSetUrl(serverAddr string) string {
	return prependHttp(serverAddr) + "/pools/default"
}

func getAddNodeUrl(serverAddr string) string {
	return getHttpsHostname(serverAddr) + "/controller/addNode"
}

func getPoolsUrl(serverAddr string) string {
	return prependHttp(serverAddr) + "/pools/default"
}

func getRebalanceUrl(serverAddr string) string {
	return prependHttp(serverAddr) + "/controller/rebalance"
}

func getRecoveryUrl(serverAddr string) string {
	return prependHttp(serverAddr) + "/controller/setRecoveryType"
}

func getTaskUrl(serverAddr string) string {
	return prependHttp(serverAddr) + "/pools/default/tasks"
}

func getFailoverUrl(serverAddr string) string {
	return prependHttp(serverAddr) + "/controller/failOver"
}

func failoverFromRest(serverAddr, username, password string, nodesToRemove []string) ([]byte, error) {
	log.Printf("Failing over: %v\n", nodesToRemove)

	_, removeNodes := otpNodes(serverAddr, username, password, nodesToRemove)
	payload := strings.NewReader(fmt.Sprintf("otpNode=%s", url.QueryEscape(removeNodes)))
	return makeRequest(username, password, "POST", payload, getFailoverUrl(serverAddr))
}

func recoveryFromRest(serverAddr, username, password, hostname, recoveryType string) ([]byte, error) {
	log.Printf("Kicking off failover recovery, type: %s\n", recoveryType)

	_, recoveryNodes := otpNodes(serverAddr, username, password, []string{hostname})
	payload := strings.NewReader(fmt.Sprintf("otpNode=%s&recoveryType=%s", url.QueryEscape(recoveryNodes), recoveryType))
	return makeRequest(username, password, "POST", payload, getRecoveryUrl(serverAddr))
}

func initServicesFromRest(serverAddr, username, password, roles string) ([]byte, error) {
	log.Printf("Initialising services with role: %s on node: %v\n", roles, serverAddr)

	payload := strings.NewReader(fmt.Sprintf("services=%s", roles))
	return makeRequest("", "", "POST", payload, getInitServicesUrl(serverAddr))
}

func initWebCredsFromRest(serverAddr, username, password string) ([]byte, error) {
	log.Printf("Initialising web UI on node: %v\n", serverAddr)

	payload := strings.NewReader(fmt.Sprintf("username=%s&password=%s&port=SAME", username, password))
	return makeRequest("", "", "POST", payload, getWebCredsUrl(serverAddr))
}

func setQuotaUsingRest(serverAddr, username, password string) ([]byte, error) {
	log.Printf("Setting data quota of 1500M and Index quota of 1500M\n")

	payload := strings.NewReader(fmt.Sprintf("memoryQuota=1500&indexMemoryQuota=1500"))
	return makeRequest(username, password, "POST", payload, getQuotaSetUrl(serverAddr))
}

func addNodeFromRest(serverAddr, username, password, hostname, roles string) ([]byte, error) {

	hostname = getHttpsHostname(hostname)
	log.Printf("Adding node: %s with role: %s to the cluster\n", hostname, roles)

	payload := strings.NewReader(fmt.Sprintf("hostname=%s&user=%s&password=%s&services=%s",
		url.QueryEscape(hostname), username, password, url.QueryEscape(roles)))
	return makeRequest(username, password, "POST", payload, getAddNodeUrl(serverAddr))
}

func rebalanceFromRest(serverAddr, username, password string, nodesToRemove []string) ([]byte, error) {
	if len(nodesToRemove) > 0 && nodesToRemove[0] != "" {
		log.Printf("Removing node(s): %v from the cluster\n", nodesToRemove)
	}

	knownNodes, removeNodes := otpNodes(serverAddr, username, password, nodesToRemove)
	payload := strings.NewReader(fmt.Sprintf("knownNodes=%s&ejectedNodes=%s",
		url.QueryEscape(knownNodes), url.QueryEscape(removeNodes)))
	return makeRequest(username, password, "POST", payload, getRebalanceUrl(serverAddr))
}

func otpNodes(serverAddr, username, password string, removeNodes []string) (string, string) {
	defer func() {
		recover()
	}()

	r, err := makeRequest(username, password, "GET", strings.NewReader(""), getPoolsUrl(serverAddr))

	var res map[string]interface{}
	err = json.Unmarshal(r, &res)
	if err != nil {
		fmt.Println("otp node fetch error", err)
	}

	nodes := res["nodes"].([]interface{})
	var ejectNodes, knownNodes string

	for i, n := range nodes {
		node := n.(map[string]interface{})
		knownNodes += node["otpNode"].(string)
		if i < len(nodes)-1 {
			knownNodes += ","
		}

		for j, en := range removeNodes {
			if en == node["hostname"].(string) {
				ejectNodes += node["otpNode"].(string)
				if j < len(removeNodes)-1 {
					ejectNodes += ","
				}
			}
		}
	}

	return knownNodes, ejectNodes
}

func waitForRebalanceFinish(serverAddr, username, password string) error {
	timer := time.NewTicker(5 * time.Second)
	timeout := time.After(30 * time.Minute)

	for {
		select {
		case <-timer.C:

			r, err := makeRequest(username, password, "GET", strings.NewReader(""), getTaskUrl(serverAddr))

			var tasks []interface{}
			err = json.Unmarshal(r, &tasks)
			if err != nil {
				fmt.Println("tasks fetch, err:", err)
				return err
			}

			for _, v := range tasks {
				task := v.(map[string]interface{})
				if task["errorMessage"] != nil {
					log.Println(task["errorMessage"].(string))
					return ErrRebalanceFailed
				}
				if task["type"].(string) == "rebalance" && task["status"].(string) == "running" {
					log.Println("Rebalance progress:", task["progress"])
				}

				if task["type"].(string) == "rebalance" && task["status"].(string) == "notRunning" {
					timer.Stop()
					log.Println("Rebalance progress: 100")
					return nil
				}
			}
			// Incase rebalance is stuck, terminate the wait after 30 minutes
		case <-timeout:
			return ErrRebalanceTimedout
		}
	}
}

func makeRequest(username, password, requestType string, payload *strings.Reader, url string) ([]byte, error) {
	req, err := http.NewRequest(requestType, url, payload)
	if err != nil {
		fmt.Println(err)
		return nil, err
	}

	req.Header.Add("content-type", "application/x-www-form-urlencoded")
	if username != "" && password != "" {
		req.SetBasicAuth(username, password)
	}

	var client *http.Client

	if len(url) > 8 && url[0:8] == "https://" {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}

		client = &http.Client{Transport: tr}
	} else {
		client = http.DefaultClient
	}

	res, err := client.Do(req)
	if err != nil {
		fmt.Println(err)
		return nil, err
	}
	defer res.Body.Close()
	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		fmt.Println(err)
		return nil, err
	}
	return data, nil
}

func GetClusterStatus(serverAddr, username, password string) map[string][]string {
	defer func() {
		recover()
	}()

	r, err := makeRequest(username, password, "GET", strings.NewReader(""), getPoolsUrl(serverAddr))

	var pool couchbase.Pool
	err = json.Unmarshal(r, &pool)
	if err != nil {
		log.Printf("otp node fetch error: %v", err)
	}

	status := make(map[string][]string)
	for _, node := range pool.Nodes {
		status[node.Hostname] = node.Services
	}
	return status
}

// AddNode just adds a node to the cluster but does NOT perform rebalance.
// It does this by calling the ns_server /controller/addNode documented REST endpoint.
// It retries up to 30 times one second apart because both the servicing node and the
// newly added node may take a long time (at least > 10 sec) to become ready to respond.
func AddNode(serverAddr, username, password, hostname string, role string) (err error) {
	method := "AddNode" // for logging
	host := prependHttp(hostname)
	var res []byte      // raw HTTP response
	var response string // string form of res
	for retries := 0; ; retries++ {
		res, err = addNodeFromRest(serverAddr, username, password, host, role)
		if err == nil {
			response = fmt.Sprintf("%s", res)
			if strings.Contains(response, "{\"otpNode\":") {
				log.Printf("%v: Successfully added node: %v (role %v), response: %v",
					method, hostname, role, response)
				return nil
			}
		}
		if retries >= 30 {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if err != nil {
		return fmt.Errorf("%v: Error from addNodeFromRest while adding node: %v (role: %v), err: %v",
			method, hostname, role, err)
	}
	return fmt.Errorf("%v: Unexpected response body from addNodeFromRest while adding node: %v (role: %v), response: %v",
		method, hostname, role, response)
}

// AddNodeAndRebalance adds a node to the cluster and then does a rebalance.
// Adding the node is delegated to AddNode.
// Rebalance is done by calling the ns_server /controller/rebalance documented REST endpoint.
func AddNodeAndRebalance(serverAddr, username, password, hostname string, role string) error {
	method := "AddNodeAndRebalance" // for logging
	err := AddNode(serverAddr, username, password, hostname, role)
	if err != nil {
		return err
	}

	if res, err := rebalanceFromRest(serverAddr, username, password, []string{""}); err != nil {
		return fmt.Errorf("%v: Error calling rebalanceFromRest, err: %v", method, err)
	} else if err == nil && res != nil && (fmt.Sprintf("%s", res) != "") {
		return fmt.Errorf("%v: Error in rebalanceFromRest response: %s", method, res)
	}

	if err := waitForRebalanceFinish(serverAddr, username, password); err != nil {
		return fmt.Errorf("%v: Error during rebalance, err: %v", method, err)
	}
	return nil
}

func InitClusterServices(serverAddr, username, password, role string) error {

	if res, err := initServicesFromRest(serverAddr, username, password, role); err != nil {
		return fmt.Errorf("Error while initialising services from REST, err: %v", err)
	} else {
		response := fmt.Sprintf("%s", res)
		if response != "" {
			return fmt.Errorf("Unexpected response while initialising cluster services response: %s", res)
		}
	}
	return nil
}

func InitWebCreds(serverAddr, username, password string) error {
	if res, err := initWebCredsFromRest(serverAddr, username, password); err != nil {
		return fmt.Errorf("Error while initialising web credentials node from REST, err: %v", err)
	} else {
		response := fmt.Sprintf("%s", res)
		log.Printf("InitWebCreds, response is: %v", response)
	}
	return nil
}

func InitDataAndIndexQuota(serverAddr, username, password string) error {
	if res, err := setQuotaUsingRest(serverAddr, username, password); err != nil {
		return fmt.Errorf("Error while setting index and data quota using REST, err: %v", err)
	} else {
		response := fmt.Sprintf("%s", res)
		if response != "" {
			return fmt.Errorf("Received error response while initialising data and index quota from REST, err: %v", err)
		}
	}
	return nil
}

// RemoveNode performs a rebalance out (ejection) of the specified node.
// This is done by calling the ns_server /controller/rebalance documented REST endpoint.
func RemoveNode(serverAddr, username, password, hostname string) error {
	if res, err := rebalanceFromRest(serverAddr, username, password, []string{hostname}); err != nil {
		return fmt.Errorf("Error while removing node and rebalance, hostname: %v, err: %v", hostname, err)
	} else if err == nil && res != nil && (fmt.Sprintf("%s", res) != "") {
		return fmt.Errorf("Error removing node and rebalancing, rebalanceFromRest response: %s", res)
	}
	if err := waitForRebalanceFinish(serverAddr, username, password); err != nil {
		return fmt.Errorf("Error during rebalance, err: %v", err)
	}
	return nil
}

func FailoverNode(serverAddr, username, password, hostname string) error {
	if res, err := failoverFromRest(serverAddr, username, password, []string{hostname}); err != nil {
		return fmt.Errorf("Error while failing over, hostname: %v, err: %v", hostname, err)
	} else if err == nil && res != nil && (fmt.Sprintf("%s", res) != "") {
		return fmt.Errorf("Error removing node and rebalancing, rebalanceFromRest response: %s", res)
	}
	return nil
}

func Rebalance(serverAddr, username, password string) error {
	if res, err := rebalanceFromRest(serverAddr, username, password, []string{""}); err != nil {
		return fmt.Errorf("Error while rebalancing, err: %v", err)
	} else if err == nil && res != nil && (fmt.Sprintf("%s", res) != "") {
		return fmt.Errorf("Error while rebalancing, rebalanceFromRest response: %s", res)
	}
	if err := waitForRebalanceFinish(serverAddr, username, password); err != nil {
		return fmt.Errorf("Error during rebalance, err: %v", err)
	}
	return nil
}

func ResetCluster(serverAddr, username, password string, dropNodes []string, keepNodes map[string]string) error {

	if res, err := rebalanceFromRest(serverAddr, username, password, dropNodes); err != nil {
		return fmt.Errorf("Error while rebalancing-out nodes %v, err: %v", dropNodes, err)
	} else if err == nil && res != nil && (fmt.Sprintf("%s", res) != "") {
		return fmt.Errorf("Error resetCluster: rebalanceFromRest, response: %s", res)
	}
	if err := waitForRebalanceFinish(serverAddr, username, password); err != nil {
		return fmt.Errorf("Error in resetCluster, err: %v", err)
	}

	for node, role := range keepNodes {
		err := AddNodeAndRebalance(serverAddr, username, password, node, role)
		if err != nil {
			return fmt.Errorf("Error while adding node: %v (role: %v) to cluster, err: %v", node, role, err)
		}
	}
	return nil
}

func IsNodeIndex(status map[string][]string, hostname string) bool {
	services := status[hostname]
	for _, service := range services {
		if service == "index" {
			return true
		}
	}
	return false
}

func IsNodeKV(status map[string][]string, hostname string) bool {
	services := status[hostname]
	for _, service := range services {
		if service == "kv" {
			return true
		}
	}
	return false
}

func IsNodeN1QL(status map[string][]string, hostname string) bool {
	services := status[hostname]
	for _, service := range services {
		if service == "n1ql" {
			return true
		}
	}
	return false
}

// This function checks if servers are active on all the "nodes"
// In cases where the rebalance tests are run without required number
// of servers in cluster_run, this validation makes sure that all the
// tests are considered PASS
func ValidateServers(serverAddr, username, password string, nodes []string) error {
	for _, node := range nodes {
		_, err := makeRequest(username, password, "GET", strings.NewReader(""), prependHttp(node))
		if err != nil {
			return err
		}
	}
	return nil
}

func prependHttp(url string) string {
	if len(url) > 7 && url[0:7] == "http://" {
		return url
	} else {
		return "http://" + url
	}
}

func prependHttps(url string) string {
	if len(url) > 8 && url[0:7] == "https://" {
		return url
	} else if len(url) > 7 && url[0:7] == "http://" {
		newUrl := "https://" + url[7:]
		return newUrl
	} else {
		return "https://" + url
	}
}

// TODO: Add more ports whenever required
var securePortMap = map[string]string{
	"9000": "19000",
	"9001": "19001",
	"9002": "19002",
	"9003": "19003",
	"8091": "18091",
	"9102": "19102",
}

func useSecurePort(hostname string) string {
	comps := strings.Split(hostname, ":")
	n := len(comps)
	if n > 0 {
		if newPort, ok := securePortMap[comps[n-1]]; ok {
			comps[n-1] = newPort
			return strings.Join(comps, ":")
		}
	}

	return hostname
}

func getHttpsHostname(hostname string) string {
	hostname = prependHttps(hostname)
	return useSecurePort(hostname)
}
