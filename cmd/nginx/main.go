/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	proxyproto "github.com/armon/go-proxyproto"
	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"k8s.io/ingress-nginx/pkg/ingress"
	"k8s.io/ingress-nginx/pkg/ingress/controller"
	"k8s.io/ingress-nginx/pkg/k8s"
	"k8s.io/ingress-nginx/pkg/net/ssl"
	"k8s.io/ingress-nginx/version"
)

func main() {
	fmt.Println(version.String())

	showVersion, conf, err := parseFlags()
	if showVersion {
		os.Exit(0)
	}

	if err != nil {
		glog.Fatal(err)
	}

	kubeClient, err := createApiserverClient(conf.APIServerHost, conf.KubeConfigFile)
	if err != nil {
		handleFatalInitError(err)
	}

	ns, name, err := k8s.ParseNameNS(conf.DefaultService)
	if err != nil {
		glog.Fatalf("invalid format for service %v: %v", conf.DefaultService, err)
	}

	_, err = kubeClient.Core().Services(ns).Get(name, metav1.GetOptions{})
	if err != nil {
		if strings.Contains(err.Error(), "cannot get services in the namespace") {
			glog.Fatalf("✖ It seems the cluster it is running with Authorization enabled (like RBAC) and there is no permissions for the ingress controller. Please check the configuration")
		}
		glog.Fatalf("no service with name %v found: %v", conf.DefaultService, err)
	}
	glog.Infof("validated %v as the default backend", conf.DefaultService)

	if conf.PublishService != "" {
		ns, name, err := k8s.ParseNameNS(conf.PublishService)
		if err != nil {
			glog.Fatalf("invalid service format: %v", err)
		}

		svc, err := kubeClient.CoreV1().Services(ns).Get(name, metav1.GetOptions{})
		if err != nil {
			glog.Fatalf("unexpected error getting information about service %v: %v", conf.PublishService, err)
		}

		if len(svc.Status.LoadBalancer.Ingress) == 0 {
			if len(svc.Spec.ExternalIPs) > 0 {
				glog.Infof("service %v validated as assigned with externalIP", conf.PublishService)
			} else {
				// We could poll here, but we instead just exit and rely on k8s to restart us
				glog.Fatalf("service %s does not (yet) have ingress points", conf.PublishService)
			}
		} else {
			glog.Infof("service %v validated as source of Ingress status", conf.PublishService)
		}
	}

	if conf.Namespace != "" {
		_, err = kubeClient.CoreV1().Namespaces().Get(conf.Namespace, metav1.GetOptions{})
		if err != nil {
			glog.Fatalf("no watchNamespace with name %v found: %v", conf.Namespace, err)
		}
	}

	if conf.ResyncPeriod.Seconds() < 10 {
		glog.Fatalf("resync period (%vs) is too low", conf.ResyncPeriod.Seconds())
	}

	// create directory that will contains the SSL Certificates
	err = os.MkdirAll(ingress.DefaultSSLDirectory, 0655)
	if err != nil {
		glog.Errorf("Failed to mkdir SSL directory: %v", err)
	}
	// create the default SSL certificate (dummy)
	sha, pem := createDefaultSSLCertificate()
	conf.FakeCertificatePath = pem
	conf.FakeCertificateSHA = sha

	conf.Client = kubeClient
	conf.DefaultIngressClass = defIngressClass

	ngx := controller.NewNGINXController(conf)

	if conf.EnableSSLPassthrough {
		setupSSLProxy(conf.ListenPorts.HTTPS, conf.ListenPorts.SSLProxy, ngx)
	}

	go handleSigterm(ngx)

	mux := http.NewServeMux()
	go registerHandlers(conf.EnableProfiling, conf.ListenPorts.Health, ngx, mux)

	ngx.Start()
}

func handleSigterm(ngx *controller.NGINXController) {
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGTERM)
	<-signalChan
	glog.Infof("Received SIGTERM, shutting down")

	exitCode := 0
	if err := ngx.Stop(); err != nil {
		glog.Infof("Error during shutdown %v", err)
		exitCode = 1
	}

	glog.Infof("Handled quit, awaiting pod deletion")
	time.Sleep(10 * time.Second)

	glog.Infof("Exiting with %v", exitCode)
	os.Exit(exitCode)
}

func setupSSLProxy(sslPort, proxyPort int, n *controller.NGINXController) {
	glog.Info("starting TLS proxy for SSL passthrough")
	n.Proxy = &controller.TCPProxy{
		Default: &controller.TCPServer{
			Hostname:      "localhost",
			IP:            "127.0.0.1",
			Port:          proxyPort,
			ProxyProtocol: true,
		},
	}

	listener, err := net.Listen("tcp", fmt.Sprintf(":%v", sslPort))
	if err != nil {
		glog.Fatalf("%v", err)
	}

	proxyList := &proxyproto.Listener{Listener: listener}

	// start goroutine that accepts tcp connections in port 443
	go func() {
		for {
			var conn net.Conn
			var err error

			if n.IsProxyProtocolEnabled {
				// we need to wrap the listener in order to decode
				// proxy protocol before handling the connection
				conn, err = proxyList.Accept()
			} else {
				conn, err = listener.Accept()
			}

			if err != nil {
				glog.Warningf("unexpected error accepting tcp connection: %v", err)
				continue
			}

			glog.V(3).Infof("remote address %s to local %s", conn.RemoteAddr(), conn.LocalAddr())
			go n.Proxy.Handle(conn)
		}
	}()
}

// createApiserverClient creates new Kubernetes Apiserver client. When kubeconfig or apiserverHost param is empty
// the function assumes that it is running inside a Kubernetes cluster and attempts to
// discover the Apiserver. Otherwise, it connects to the Apiserver specified.
//
// apiserverHost param is in the format of protocol://address:port/pathPrefix, e.g.http://localhost:8001.
// kubeConfig location of kubeconfig file
func createApiserverClient(apiserverHost string, kubeConfig string) (*kubernetes.Clientset, error) {
	cfg, err := buildConfigFromFlags(apiserverHost, kubeConfig)
	if err != nil {
		return nil, err
	}

	cfg.QPS = defaultQPS
	cfg.Burst = defaultBurst
	cfg.ContentType = "application/vnd.kubernetes.protobuf"

	glog.Infof("Creating API client for %s", cfg.Host)

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	v, err := client.Discovery().ServerVersion()
	if err != nil {
		return nil, err
	}

	glog.Infof("Running in Kubernetes Cluster version v%v.%v (%v) - git (%v) commit %v - platform %v",
		v.Major, v.Minor, v.GitVersion, v.GitTreeState, v.GitCommit, v.Platform)

	return client, nil
}

const (
	// High enough QPS to fit all expected use cases. QPS=0 is not set here, because
	// client code is overriding it.
	defaultQPS = 1e6
	// High enough Burst to fit all expected use cases. Burst=0 is not set here, because
	// client code is overriding it.
	defaultBurst = 1e6

	fakeCertificate = "default-fake-certificate"
)

// buildConfigFromFlags builds REST config based on master URL and kubeconfig path.
// If both of them are empty then in cluster config is used.
func buildConfigFromFlags(masterURL, kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath == "" && masterURL == "" {
		kubeconfig, err := rest.InClusterConfig()
		if err != nil {
			return nil, err
		}

		return kubeconfig, nil
	}

	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath},
		&clientcmd.ConfigOverrides{
			ClusterInfo: clientcmdapi.Cluster{
				Server: masterURL,
			},
		}).ClientConfig()
}

/**
 * Handles fatal init error that prevents server from doing any work. Prints verbose error
 * message and quits the server.
 */
func handleFatalInitError(err error) {
	glog.Fatalf("Error while initializing connection to Kubernetes apiserver. "+
		"This most likely means that the cluster is misconfigured (e.g., it has "+
		"invalid apiserver certificates or service accounts configuration). Reason: %s\n"+
		"Refer to the troubleshooting guide for more information: "+
		"https://github.com/kubernetes/ingress-nginx/blob/master/docs/troubleshooting.md", err)
}

func registerHandlers(enableProfiling bool, port int, ic *controller.NGINXController, mux *http.ServeMux) {
	// expose health check endpoint (/healthz)
	healthz.InstallHandler(mux,
		healthz.PingHealthz,
		ic,
	)

	mux.Handle("/metrics", promhttp.Handler())

	mux.HandleFunc("/build", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		b, _ := json.Marshal(version.String())
		w.Write(b)
	})

	mux.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) {
		err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		if err != nil {
			glog.Errorf("unexpected error: %v", err)
		}
	})

	if enableProfiling {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	server := &http.Server{
		Addr:    fmt.Sprintf(":%v", port),
		Handler: mux,
	}
	glog.Fatal(server.ListenAndServe())
}

func createDefaultSSLCertificate() (string, string) {
	defCert, defKey := ssl.GetFakeSSLCert()
	c, err := ssl.AddOrUpdateCertAndKey(fakeCertificate, defCert, defKey, []byte{})
	if err != nil {
		glog.Fatalf("Error generating self signed certificate: %v", err)
	}

	return c.PemSHA, c.PemFileName
}