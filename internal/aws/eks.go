package aws

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/k3s"
)

// EKS: a real Kubernetes cluster behind the AWS API.
//
// The cluster itself is the shared k3s engine (internal/engines/k3s), the
// same one AKS uses — `kubectl get nodes` talks to a real API server in a
// container, not to anything Nimbus wrote. What is AWS-shaped is only the
// surface: REST-JSON instead of ARM, an ARN instead of a resource path,
// and node groups instead of agent pools.
//
// Because the engine tags its mirrored pods with the cloud it belongs to,
// the visualizer's Kubernetes view draws an EKS cluster with no new UI
// code at all. That was the point of lifting it.

const (
	typeEKSCluster   = "AWS::EKS::Cluster"
	typeEKSNodegroup = "AWS::EKS::Nodegroup"
)

// eksCluster is the stored shape, which is also most of what
// DescribeCluster returns.
type eksCluster struct {
	Name      string            `json:"name"`
	Arn       string            `json:"arn"`
	CreatedAt float64           `json:"createdAt"`
	Version   string            `json:"version"`
	Endpoint  string            `json:"endpoint"`
	RoleArn   string            `json:"roleArn"`
	Status    string            `json:"status"`
	Tags      map[string]string `json:"tags,omitempty"`

	ResourcesVpcConfig      eksVpcConfig   `json:"resourcesVpcConfig"`
	CertificateAuthority    eksCertificate `json:"certificateAuthority"`
	KubernetesNetworkConfig map[string]any `json:"kubernetesNetworkConfig,omitempty"`
	PlatformVersion         string         `json:"platformVersion"`

	// Why the cluster is not ACTIVE, when it is not. The provider shows
	// this, and a cluster stuck in CREATING with no reason is the worst
	// thing a local emulator can do to someone's afternoon.
	Health map[string]any `json:"health,omitempty"`
}

type eksVpcConfig struct {
	SubnetIds              []string `json:"subnetIds"`
	SecurityGroupIds       []string `json:"securityGroupIds,omitempty"`
	VpcId                  string   `json:"vpcId,omitempty"`
	EndpointPublicAccess   bool     `json:"endpointPublicAccess"`
	EndpointPrivateAccess  bool     `json:"endpointPrivateAccess"`
	PublicAccessCidrs      []string `json:"publicAccessCidrs,omitempty"`
	ClusterSecurityGroupId string   `json:"clusterSecurityGroupId,omitempty"`
}

type eksCertificate struct {
	Data string `json:"data"`
}

type eksNodegroup struct {
	NodegroupName string            `json:"nodegroupName"`
	NodegroupArn  string            `json:"nodegroupArn"`
	ClusterName   string            `json:"clusterName"`
	Status        string            `json:"status"`
	CreatedAt     float64           `json:"createdAt"`
	ModifiedAt    float64           `json:"modifiedAt"`
	NodeRole      string            `json:"nodeRole"`
	Subnets       []string          `json:"subnets"`
	InstanceTypes []string          `json:"instanceTypes,omitempty"`
	AmiType       string            `json:"amiType"`
	CapacityType  string            `json:"capacityType"`
	DiskSize      int               `json:"diskSize"`
	ScalingConfig eksScalingConfig  `json:"scalingConfig"`
	Labels        map[string]string `json:"labels,omitempty"`
	Tags          map[string]string `json:"tags,omitempty"`
	Version       string            `json:"version"`
}

type eksScalingConfig struct {
	MinSize     int `json:"minSize"`
	MaxSize     int `json:"maxSize"`
	DesiredSize int `json:"desiredSize"`
}

func (s *Server) eksClusterARN(name string) string {
	return arn("eks", s.cfg.Region, s.cfg.AccountID, "cluster/"+name)
}

func (s *Server) eksNodegroupARN(cluster, name string) string {
	return arn("eks", s.cfg.Region, s.cfg.AccountID, "nodegroup/"+cluster+"/"+name)
}

// serveEKS routes on the path, which for once actually says what is
// wanted: /clusters, /clusters/{name}, /clusters/{name}/node-groups/{ng}.
func (s *Server) serveEKS(w http.ResponseWriter, r *http.Request) string {
	parts := pathParts(r.URL.Path)
	var aerr *apiError

	switch {
	case len(parts) == 1 && parts[0] == "clusters" && r.Method == http.MethodPost:
		aerr = s.createEKSCluster(w, r)
	case len(parts) == 1 && parts[0] == "clusters" && r.Method == http.MethodGet:
		aerr = s.listEKSClusters(w)
	case len(parts) == 2 && parts[0] == "clusters" && r.Method == http.MethodGet:
		aerr = s.describeEKSCluster(w, parts[1])
	case len(parts) == 2 && parts[0] == "clusters" && r.Method == http.MethodDelete:
		aerr = s.deleteEKSCluster(w, parts[1])

	case len(parts) == 3 && parts[0] == "clusters" && parts[2] == "node-groups" && r.Method == http.MethodPost:
		aerr = s.createNodegroup(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "clusters" && parts[2] == "node-groups" && r.Method == http.MethodGet:
		aerr = s.listNodegroups(w, parts[1])
	case len(parts) == 4 && parts[0] == "clusters" && parts[2] == "node-groups" && r.Method == http.MethodGet:
		aerr = s.describeNodegroup(w, parts[1], parts[3])
	case len(parts) == 4 && parts[0] == "clusters" && parts[2] == "node-groups" && r.Method == http.MethodDelete:
		aerr = s.deleteNodegroup(w, parts[1], parts[3])

	default:
		aerr = &apiError{Status: 404, Code: "ResourceNotFoundException",
			Message: fmt.Sprintf("Nimbus does not implement %s %s for EKS yet", r.Method, r.URL.Path)}
	}

	if aerr != nil {
		writeJSONError(w, aerr)
		return "eks " + r.Method + " " + r.URL.Path + ": " + aerr.Code
	}
	return ""
}

// --- clusters -------------------------------------------------------------

func (s *Server) loadEKSCluster(name string) (*eksCluster, *apiError) {
	res, err := s.store.Get(s.eksClusterARN(name))
	if err != nil || res.Type != typeEKSCluster {
		return nil, notFoundJSON("cluster", name)
	}
	var c eksCluster
	if err := json.Unmarshal(res.Body, &c); err != nil {
		return nil, &apiError{Status: 500, Code: "ServerException", Message: err.Error()}
	}
	return &c, nil
}

func (s *Server) saveEKSCluster(c *eksCluster) *apiError {
	body, err := json.Marshal(c)
	if err != nil {
		return &apiError{Status: 500, Code: "ServerException", Message: err.Error()}
	}
	s.store.Put(&core.Resource{
		Cloud: "aws", ID: c.Arn, Type: typeEKSCluster, Name: c.Name,
		ParentID: s.accountARN(), Location: s.cfg.Region, Backing: "real", Body: body,
	})
	return nil
}

func (s *Server) createEKSCluster(w http.ResponseWriter, r *http.Request) *apiError {
	var req struct {
		Name               string            `json:"name"`
		Version            string            `json:"version"`
		RoleArn            string            `json:"roleArn"`
		ResourcesVpcConfig eksVpcConfig      `json:"resourcesVpcConfig"`
		Tags               map[string]string `json:"tags"`
	}
	if err := readJSON(r, &req); err != nil {
		return &apiError{Status: 400, Code: "InvalidParameterException", Message: err.Error()}
	}
	if req.Name == "" {
		return &apiError{Status: 400, Code: "InvalidParameterException", Message: "name is required"}
	}
	if s.store.Exists(s.eksClusterARN(req.Name)) {
		return &apiError{Status: 409, Code: "ResourceInUseException",
			Message: "Cluster already exists with name: " + req.Name}
	}

	c := &eksCluster{
		Name:               req.Name,
		Arn:                s.eksClusterARN(req.Name),
		CreatedAt:          epoch(),
		Version:            orDefault(req.Version, s.k3s.Version()),
		RoleArn:            req.RoleArn,
		Status:             "CREATING",
		Tags:               req.Tags,
		ResourcesVpcConfig: req.ResourcesVpcConfig,
		PlatformVersion:    "eks.1",
	}
	c.ResourcesVpcConfig.EndpointPublicAccess = true
	if aerr := s.saveEKSCluster(c); aerr != nil {
		return aerr
	}

	// Starting a cluster takes a minute or two, and the provider polls
	// DescribeCluster until it reports ACTIVE -- so answer now and finish
	// in the background, as the ARM side does with its Provision hook.
	go s.provisionEKSCluster(c.Name)

	writeJSON(w, 200, map[string]any{"cluster": c})
	return nil
}

// provisionEKSCluster starts the k3s container and moves the cluster to
// ACTIVE, or to FAILED with the reason attached.
func (s *Server) provisionEKSCluster(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	fail := func(reason string) {
		c, aerr := s.loadEKSCluster(name)
		if aerr != nil {
			return
		}
		c.Status = "FAILED"
		c.Health = map[string]any{"issues": []any{map[string]any{
			"code": "ConfigurationConflict", "message": reason,
		}}}
		_ = s.saveEKSCluster(c)
	}

	c, aerr := s.loadEKSCluster(name)
	if aerr != nil {
		return
	}
	cl, err := s.k3s.NewCluster(c.Arn, name)
	if err != nil {
		fail(err.Error())
		return
	}
	if err := s.k3s.Start(ctx, cl); err != nil {
		s.k3s.Remove(c.Arn)
		fail(err.Error())
		return
	}

	// Re-read: the cluster may have been deleted while it was starting.
	c, aerr = s.loadEKSCluster(name)
	if aerr != nil {
		s.k3s.Stop(ctx, cl)
		return
	}
	c.Status = "ACTIVE"
	c.Endpoint = fmt.Sprintf("https://127.0.0.1:%d", cl.Port)
	c.CertificateAuthority = eksCertificate{Data: base64.StdEncoding.EncodeToString(cl.CA)}
	_ = s.saveEKSCluster(c)
}

func (s *Server) describeEKSCluster(w http.ResponseWriter, name string) *apiError {
	c, aerr := s.loadEKSCluster(name)
	if aerr != nil {
		return aerr
	}
	writeJSON(w, 200, map[string]any{"cluster": c})
	return nil
}

func (s *Server) listEKSClusters(w http.ResponseWriter) *apiError {
	names := []string{}
	for _, res := range s.store.List(func(r *core.Resource) bool { return r.Type == typeEKSCluster }) {
		names = append(names, res.Name)
	}
	writeJSON(w, 200, map[string]any{"clusters": names})
	return nil
}

func (s *Server) deleteEKSCluster(w http.ResponseWriter, name string) *apiError {
	c, aerr := s.loadEKSCluster(name)
	if aerr != nil {
		return aerr
	}
	// The container goes in the background; the API answers now with the
	// cluster in DELETING, which is what the provider polls on.
	c.Status = "DELETING"
	_ = s.saveEKSCluster(c)
	go func(arn, name string) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		cl := s.k3s.Remove(arn)
		if cl == nil {
			cl = s.k3s.ClusterFor(arn, name)
		}
		s.k3s.Stop(ctx, cl)
		_, _ = s.store.Delete(arn)
	}(c.Arn, c.Name)

	writeJSON(w, 200, map[string]any{"cluster": c})
	return nil
}

// --- node groups ----------------------------------------------------------

func (s *Server) loadNodegroup(cluster, name string) (*eksNodegroup, *apiError) {
	res, err := s.store.Get(s.eksNodegroupARN(cluster, name))
	if err != nil || res.Type != typeEKSNodegroup {
		return nil, notFoundJSON("nodegroup", name)
	}
	var ng eksNodegroup
	if err := json.Unmarshal(res.Body, &ng); err != nil {
		return nil, &apiError{Status: 500, Code: "ServerException", Message: err.Error()}
	}
	return &ng, nil
}

func (s *Server) saveNodegroup(ng *eksNodegroup) *apiError {
	body, err := json.Marshal(ng)
	if err != nil {
		return &apiError{Status: 500, Code: "ServerException", Message: err.Error()}
	}
	s.store.Put(&core.Resource{
		Cloud: "aws", ID: ng.NodegroupArn, Type: typeEKSNodegroup, Name: ng.NodegroupName,
		ParentID: s.eksClusterARN(ng.ClusterName), Location: s.cfg.Region,
		Backing: "real", Body: body,
	})
	return nil
}

func (s *Server) createNodegroup(w http.ResponseWriter, r *http.Request, cluster string) *apiError {
	if _, aerr := s.loadEKSCluster(cluster); aerr != nil {
		return aerr
	}
	var req struct {
		NodegroupName string            `json:"nodegroupName"`
		NodeRole      string            `json:"nodeRole"`
		Subnets       []string          `json:"subnets"`
		InstanceTypes []string          `json:"instanceTypes"`
		AmiType       string            `json:"amiType"`
		CapacityType  string            `json:"capacityType"`
		DiskSize      int               `json:"diskSize"`
		ScalingConfig eksScalingConfig  `json:"scalingConfig"`
		Labels        map[string]string `json:"labels"`
		Tags          map[string]string `json:"tags"`
		Version       string            `json:"version"`
	}
	if err := readJSON(r, &req); err != nil {
		return &apiError{Status: 400, Code: "InvalidParameterException", Message: err.Error()}
	}
	if req.NodegroupName == "" {
		return &apiError{Status: 400, Code: "InvalidParameterException", Message: "nodegroupName is required"}
	}
	ng := &eksNodegroup{
		NodegroupName: req.NodegroupName,
		NodegroupArn:  s.eksNodegroupARN(cluster, req.NodegroupName),
		ClusterName:   cluster,
		Status:        "CREATING",
		CreatedAt:     epoch(),
		ModifiedAt:    epoch(),
		NodeRole:      req.NodeRole,
		Subnets:       req.Subnets,
		InstanceTypes: req.InstanceTypes,
		AmiType:       orDefault(req.AmiType, "AL2_x86_64"),
		CapacityType:  orDefault(req.CapacityType, "ON_DEMAND"),
		DiskSize:      orZero(req.DiskSize, 20),
		ScalingConfig: req.ScalingConfig,
		Labels:        req.Labels,
		Tags:          req.Tags,
		Version:       orDefault(req.Version, s.k3s.Version()),
	}
	if ng.ScalingConfig.DesiredSize == 0 {
		ng.ScalingConfig = eksScalingConfig{MinSize: 1, MaxSize: 1, DesiredSize: 1}
	}
	if aerr := s.saveNodegroup(ng); aerr != nil {
		return aerr
	}
	go s.provisionNodegroup(cluster, ng.NodegroupName)

	writeJSON(w, 200, map[string]any{"nodegroup": ng})
	return nil
}

// provisionNodegroup grows the cluster to the desired size, waits for the
// nodes to REGISTER, and then reports ACTIVE.
//
// Waiting for registration rather than for containers is the lesson AKS
// learned: anyone who runs kubectl straight after `terraform apply` would
// otherwise see fewer nodes than they asked for, or none.
func (s *Server) provisionNodegroup(cluster, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	ng, aerr := s.loadNodegroup(cluster, name)
	if aerr != nil {
		return
	}
	cl := s.k3s.Get(s.eksClusterARN(cluster))
	if cl == nil {
		ng.Status = "CREATE_FAILED"
		_ = s.saveNodegroup(ng)
		return
	}
	// The server node counts towards the total, the way an AKS pool's
	// count does: a node group of one on a one-node cluster adds nothing.
	want := ng.ScalingConfig.DesiredSize
	if err := s.k3s.ScaleAgents(ctx, cl, want); err != nil {
		ng.Status = "CREATE_FAILED"
		_ = s.saveNodegroup(ng)
		return
	}
	if err := s.k3s.WaitForNodes(ctx, cl, want); err != nil {
		ng.Status = "CREATE_FAILED"
		_ = s.saveNodegroup(ng)
		return
	}
	ng, aerr = s.loadNodegroup(cluster, name)
	if aerr != nil {
		return
	}
	ng.Status = "ACTIVE"
	ng.ModifiedAt = epoch()
	_ = s.saveNodegroup(ng)
}

func (s *Server) describeNodegroup(w http.ResponseWriter, cluster, name string) *apiError {
	ng, aerr := s.loadNodegroup(cluster, name)
	if aerr != nil {
		return aerr
	}
	writeJSON(w, 200, map[string]any{"nodegroup": ng})
	return nil
}

func (s *Server) listNodegroups(w http.ResponseWriter, cluster string) *apiError {
	if _, aerr := s.loadEKSCluster(cluster); aerr != nil {
		return aerr
	}
	names := []string{}
	prefix := s.eksNodegroupARN(cluster, "")
	for _, res := range s.store.List(func(r *core.Resource) bool {
		return r.Type == typeEKSNodegroup && strings.HasPrefix(r.ID, prefix)
	}) {
		names = append(names, res.Name)
	}
	writeJSON(w, 200, map[string]any{"nodegroups": names})
	return nil
}

func (s *Server) deleteNodegroup(w http.ResponseWriter, cluster, name string) *apiError {
	ng, aerr := s.loadNodegroup(cluster, name)
	if aerr != nil {
		return aerr
	}
	ng.Status = "DELETING"
	_ = s.saveNodegroup(ng)
	go func(arn string) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		// Shrink back to the server node alone; the cluster itself stays.
		if cl := s.k3s.Get(s.eksClusterARN(cluster)); cl != nil {
			_ = s.k3s.ScaleAgents(ctx, cl, 1)
		}
		_, _ = s.store.Delete(arn)
	}(ng.NodegroupArn)

	writeJSON(w, 200, map[string]any{"nodegroup": ng})
	return nil
}

// epoch is the fractional Unix seconds AWS timestamps use in JSON.
func epoch() float64 {
	return float64(time.Now().UnixMilli()) / 1000
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func orZero(v, fallback int) int {
	if v == 0 {
		return fallback
	}
	return v
}

// eksClusters returns every cluster the engine should still be running,
// for the shutdown path.
func (s *Server) eksClusters() []*k3s.Cluster { return s.k3s.All() }
