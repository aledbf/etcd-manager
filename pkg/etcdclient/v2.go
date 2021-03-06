package etcdclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	etcd_client "github.com/coreos/etcd/client"
	"github.com/golang/glog"
)

type Client interface {
	ListMembers(ctx context.Context) ([]*EtcdProcessMember, error)
}

type etcdClient struct {
	ClientURL string
}

var _ Client = &etcdClient{}

type EtcdProcessMember struct {
	Id         string   `json:"id,omitempty"`
	Name       string   `json:"name,omitempty"`
	PeerURLs   []string `json:"peerURLs,omitempty"`
	ClientURLs []string `json:"clientURLs,omitempty"`
}

func (m *EtcdProcessMember) Client() (etcd_client.Client, error) {
	cfg := etcd_client.Config{
		Endpoints: m.ClientURLs,
		Transport: etcd_client.DefaultTransport,
		// set timeout per request to fail fast when the target endpoint is unavailable
		HeaderTimeoutPerRequest: time.Second,
	}
	etcdClient, err := etcd_client.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("error building etcd client for %s: %v", m.Name, err)
	}
	return etcdClient, nil

}
func (m *EtcdProcessMember) String() string {
	s, err := json.Marshal(m)
	if err != nil {
		return fmt.Sprintf("<error marshallling: %v>", err)
	}
	return string(s)
}

type etcdProcessMemberList struct {
	Members []*EtcdProcessMember `json:"members"`
}

func NewClient(clientURL string) Client {
	return &etcdClient{
		ClientURL: clientURL,
	}
}

func (e *etcdClient) ListMembers(ctx context.Context) ([]*EtcdProcessMember, error) {
	client := &http.Client{}
	method := "GET"
	url := fmt.Sprintf("%s/v2/members", e.ClientURL)
	request, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, fmt.Errorf("error building etcd request %s %s: %v", method, url, err)
	}
	request = request.WithContext(ctx)
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("error performing etcd request %s %s: %v", method, url, err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected response querying etcd members %s %s: %s", method, url, response.Status)
	}

	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading etcd response %s %s: %v", method, url, err)
	}
	members := &etcdProcessMemberList{}
	if err := json.Unmarshal(body, &members); err != nil {
		glog.Infof("invalid etcd response: %q", string(body))
		return nil, fmt.Errorf("error parsing etcd response %s %s: %v", method, url, err)
	}
	return members.Members, nil
}

//func (e *etcdClient) AddMember(ctx context.Context, name string, peerURLs []string) (*EtcdProcessMember, error) {
//	if name == "" {
//		glog.Fatalf("attempt to add member with no name")
//	}
//
//	client := &http.Client{}
//
//	m := &EtcdProcessMember{
//		Name:     name,
//		PeerURLs: peerURLs,
//	}
//	postBody, err := json.Marshal(m)
//	if err != nil {
//		return nil, fmt.Errorf("error building payload for member-add: %v", err)
//	}
//	method := "POST"
//	url := fmt.Sprintf("%s/v2/members", e.ClientURL)
//	request, err := http.NewRequest("POST", url, bytes.NewReader(postBody))
//	if err != nil {
//		return nil, fmt.Errorf("error building etcd request %s %s: %v", method, url, err)
//	}
//	request.Header.Add("Content-Type", "application/json")
//	request = request.WithContext(ctx)
//	response, err := client.Do(request)
//	if err != nil {
//		return nil, fmt.Errorf("error performing etcd request %s %s: %v", method, url, err)
//	}
//	defer response.Body.Close()
//	if response.StatusCode != 201 {
//		glog.Infof("POSTed content was %q", string(postBody))
//		return nil, fmt.Errorf("unexpected response adding etcd member %s %s: %s", method, url, response.Status)
//	}
//
//	body, err := ioutil.ReadAll(response.Body)
//	if err != nil {
//		return nil, fmt.Errorf("error reading etcd response %s %s: %v", method, url, err)
//	}
//	member := &EtcdProcessMember{}
//	if err := json.Unmarshal(body, &member); err != nil {
//		glog.Infof("invalid etcd response: %q", string(body))
//		return nil, fmt.Errorf("error parsing etcd response %s %s: %v", method, url, err)
//	}
//	glog.Infof("created etcd member: %v", member)
//	return member, nil
//}
