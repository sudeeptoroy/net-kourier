package kubernetes

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	gatewayLabelSelector  = "app=3scale-kourier-gateway"
	httpClientTimeout     = 2 * time.Second
	gatewaySyncTimeout    = 3 * time.Second
	httpPortInternal      = uint32(8081)
	internalKourierPath   = "/__internalkouriersnapshot"
	internalKourierDomain = "internalkourier"
	internalKourierHeader = "kourier-snapshot-id"
)

var (
	inSync int
	wg     sync.WaitGroup
	mutex  sync.Mutex
)

func (kubernetesClient *KubernetesClient) GetKourierGatewayPODS(namespace string) (*v1.PodList, error) {
	opts := meta_v1.ListOptions{
		LabelSelector: gatewayLabelSelector,
	}
	pods, err := kubernetesClient.Client.CoreV1().Pods(namespace).List(opts)
	if err != nil {
		return &v1.PodList{}, err
	}

	return pods, nil
}

func (kubernetesClient *KubernetesClient) CheckGatewaySnapshot(gwPods *v1.PodList, snapshotID string) (bool, error) {
	var ips []string

	for _, pod := range gwPods.Items {
		if pod.Status.PodIP != "" {
			ips = append(ips, pod.Status.PodIP)
		}
	}

	if len(ips) == 0 {
		return false, nil
	}

	inSync = 0
	wg.Add(len(ips))

	// Golang http.Client has keepalive by default to true, we don't want it here, or we will be always hitting the
	// draining cluster, and, getting the previous revision.
	tr := &http.Transport{
		DisableKeepAlives: true,
	}
	client := http.Client{
		Transport: tr,
		Timeout:   httpClientTimeout,
	}

	for _, ip := range ips {

		go func() {
			defer wg.Done()

			currentSnapshot, err := getCurrentGWSnapshot(ip, client)
			if err != nil {
				logrus.Errorf("Failed getting the current GW snapshot: %s for gw: %s", err, ip)
				return
			}
			if currentSnapshot == snapshotID {
				mutex.Lock()
				inSync++
				mutex.Unlock()
			}
		}()
	}
	if waitTimeout(&wg, gatewaySyncTimeout) {
		return false, nil
	}

	return inSync == len(ips), nil
}

func getCurrentGWSnapshot(ip string, client http.Client) (string, error) {

	req, err := buildInternalKourierRequest(ip)
	if err != nil {
		return "", err
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}

	if resp.StatusCode == http.StatusOK {
		return resp.Header.Get(internalKourierHeader), nil
	}

	return "", fmt.Errorf("status code %d", resp.StatusCode)
}

// waitTimeout waits for the waitgroup for the specified max timeout.
// Returns true if waiting timed out.
func waitTimeout(wg *sync.WaitGroup, timeout time.Duration) bool {
	c := make(chan struct{})
	go func() {
		defer close(c)
		wg.Wait()
	}()
	select {
	case <-c:
		return false // completed normally
	case <-time.After(timeout):
		return true // timed out
	}
}

func buildInternalKourierRequest(ip string) (*http.Request, error) {

	port := strconv.Itoa(int(httpPortInternal))
	req, err := http.NewRequest("GET", "http://"+ip+":"+port+internalKourierPath, nil)
	if err != nil {
		return &http.Request{}, err
	}
	req.Host = internalKourierDomain

	return req, nil
}