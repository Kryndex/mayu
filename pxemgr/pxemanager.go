package pxemgr

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"

	mayuerror "github.com/giantswarm/mayu/error"
	"github.com/giantswarm/mayu/hostmgr"
	"github.com/giantswarm/mayu/logging"
	"github.com/golang/glog"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/url"
	"strconv"
)

type PXEManagerConfiguration struct {
	ConfigFile               string
	UseInternalEtcdDiscovery bool
	EtcdQuorumSize           int
	EtcdDiscoveryUrl         string
	EtcdEndpoint             string
	EtcdCAFile               string
	DNSmasqExecutable        string
	DNSmasqTemplate          string
	TFTPRoot                 string
	NoTLS                    bool
	TLSCertFile              string
	TLSKeyFile               string
	APIPort                  int
	PXEPort                  int
	BindAddress              string
	FileServerPath           string
	StaticHTMLPath           string
	TemplateSnippets         string
	IgnitionConfig           string
	ImagesCacheDir           string
	FilesDir                 string
	Version                  string
}

type pxeManagerT struct {
	noTLS                    bool
	apiPort                  int
	pxePort                  int
	bindAddress              string
	tlsCertFile              string
	tlsKeyFile               string
	fileServerPath           string
	staticHTMLPath           string
	templateSnippets         string
	ignitionConfig           string
	imagesCacheDir           string
	filesDir                 string
	useInternalEtcdDiscovery bool
	defaultEtcdQuorumSize    int
	etcdDiscoveryUrl         string
	etcdEndpoint             string
	etcdCAFile               string
	version                  string
	configFile               string

	config  *Configuration
	cluster *hostmgr.Cluster
	DNSmasq *DNSmasqInstance

	mu *sync.Mutex

	apiRouter *mux.Router
	pxeRouter *mux.Router
}

func PXEManager(c PXEManagerConfiguration, cluster *hostmgr.Cluster) (*pxeManagerT, error) {
	conf, err := LoadConfig(c.ConfigFile)
	if err != nil {
		glog.Fatalln(err)
	}

	if conf.DefaultCoreOSVersion == "" {
		glog.Fatalf("No default_coreos_version specified in %s\n", c.ConfigFile)
	}

	if c.APIPort == c.PXEPort {
		glog.Fatalln("API port and PXE port cannot be same")
	}

	if c.EtcdDiscoveryUrl != "" && c.UseInternalEtcdDiscovery {
		glog.Fatalln("External etcd discovery url is set and internal etcd discovery is activated. Please choose only one.")
	} else if c.EtcdDiscoveryUrl == "" && !c.UseInternalEtcdDiscovery {
		glog.Fatalln("The internal etcd discovery is deactivated and no external discovery url is given")
	} else if c.UseInternalEtcdDiscovery && c.EtcdEndpoint == "" {
		glog.Fatalln("The internal etcd discovery is activated but no etcd endpoint is given")
	}

	c.EtcdDiscoveryUrl = strings.TrimRight(c.EtcdDiscoveryUrl, "/")

	mgr := &pxeManagerT{
		noTLS:                    c.NoTLS,
		apiPort:                  c.APIPort,
		pxePort:                  c.PXEPort,
		bindAddress:              c.BindAddress,
		tlsCertFile:              c.TLSCertFile,
		tlsKeyFile:               c.TLSKeyFile,
		fileServerPath:           c.FileServerPath,
		staticHTMLPath:           c.StaticHTMLPath,
		templateSnippets:         c.TemplateSnippets,
		ignitionConfig:           c.IgnitionConfig,
		imagesCacheDir:           c.ImagesCacheDir,
		filesDir:                 c.FilesDir,
		useInternalEtcdDiscovery: c.UseInternalEtcdDiscovery,
		defaultEtcdQuorumSize:    c.EtcdQuorumSize,
		etcdDiscoveryUrl:         c.EtcdDiscoveryUrl,
		etcdEndpoint:             c.EtcdEndpoint,
		etcdCAFile:               c.EtcdCAFile,
		configFile:               c.ConfigFile,
		version:                  c.Version,

		config:  &conf,
		cluster: cluster,
		DNSmasq: NewDNSmasq("/tmp/dnsmasq.mayu", DNSmasqConfiguration{
			Executable: c.DNSmasqExecutable,
			Template:   c.DNSmasqTemplate,
			TFTPRoot:   c.TFTPRoot,
			PXEPort:    c.PXEPort,
		}),
		mu: new(sync.Mutex),
	}

	// check for deprecated EtcdDiscoveryUrl
	if mgr.cluster.Config.EtcdDiscoveryURL != "" && mgr.cluster.Config.DefaultEtcdClusterToken == "" {
		// transform discovery url to token
		parts := strings.Split(mgr.cluster.Config.EtcdDiscoveryURL, "/")
		token := parts[len(parts)-1]
		baseUrl := strings.Join(parts[:len(parts)-1], "/")

		if mgr.useInternalEtcdDiscovery {
			// convert token to internal etcd discovery
			err := mgr.cluster.StoreEtcdDiscoveryToken(mgr.etcdEndpoint, mgr.etcdCAFile, token, mgr.defaultEtcdQuorumSize)
			if err != nil {
				glog.Fatal("Can't store discovery token in etcd.", baseUrl, mgr.etcdDiscoveryUrl)
			}

			glog.Warningf("Transferred etcd token to internal discovery. Note that your machines still have the old discovery url in their cloud-config and that you need to transfer the current member data yourself.")
		} else if mgr.etcdDiscoveryUrl != baseUrl {
			glog.Fatalf("Deprecated EtcdDiscoveryURL ('%s') in your cluster.json differs from the --etcd-discovery parameter ('%s').", baseUrl, mgr.etcdDiscoveryUrl)
		}
		mgr.cluster.Config.EtcdDiscoveryURL = ""
		mgr.cluster.Config.DefaultEtcdClusterToken = token
		mgr.cluster.Commit(fmt.Sprintf("Convert deprecated etcd discovery url to default etcd token '%s'", token))
	}

	if mgr.cluster.Config.DefaultEtcdClusterToken == "" {
		var (
			token string
			err   error
		)
		if mgr.useInternalEtcdDiscovery {
			token, err = mgr.cluster.GenerateEtcdDiscoveryToken()
			if err != nil {
				glog.Fatalf("Failed to generate etcd cluster token: %s", err)
			}
			err := mgr.cluster.StoreEtcdDiscoveryToken(mgr.etcdEndpoint, mgr.etcdCAFile, token, mgr.defaultEtcdQuorumSize)
			if err != nil {
				glog.Fatalf("Failed to store etcd cluster token in etcd: %s", err)
			}
		} else {
			token, err = mgr.cluster.FetchEtcdDiscoveryToken(mgr.etcdDiscoveryUrl, mgr.defaultEtcdQuorumSize)
			if err != nil {
				glog.Fatalf("Failed to fetch etcd cluster token from external registry: %s", err)
			}
		}
		mgr.cluster.Config.DefaultEtcdClusterToken = token
		mgr.cluster.Commit(fmt.Sprintf("Set default etcd cluster to '%s'", token))
	}

	if mgr.useInternalEtcdDiscovery {
		mgr.etcdDiscoveryUrl = mgr.config.TemplatesEnv["mayu_https_endpoint"].(string) + "/etcd"
	}

	return mgr, nil
}

func withSerialParam(serialHandler func(serial string, w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params := mux.Vars(r)
		serialHandler(params["serial"], w, r)
	}
}

func (mgr *pxeManagerT) startIPXEserver() error {
	mgr.pxeRouter = mux.NewRouter()

	// ipxe script
	mgr.pxeRouter.Methods("GET").PathPrefix("/ipxebootscript").HandlerFunc(mgr.ipxeBootScript)

	// get ignition
	mgr.pxeRouter.Methods("GET").PathPrefix("/ignition").HandlerFunc(mgr.ignitionGenerator)

	// endpoint for fetching coreos images defined by machine serial number
	mgr.pxeRouter.Methods("GET").PathPrefix("/images/{serial}").HandlerFunc(mgr.imagesHandler)

	// serve static files like
	mgr.pxeRouter.PathPrefix("/").Handler(http.FileServer(http.Dir(mgr.staticHTMLPath)))

	// add welcome handler for debugging
	mgr.pxeRouter.Path("/").HandlerFunc(mgr.welcomeHandler)

	glogWrapper := logging.NewGlogWrapper(8)
	loggedRouter := handlers.LoggingHandler(glogWrapper, mgr.pxeRouter)

	glog.V(8).Infoln(fmt.Sprintf("starting iPXE server at %s:%d", mgr.bindAddress, mgr.pxePort))

	err := http.ListenAndServe(net.JoinHostPort(mgr.bindAddress, strconv.Itoa(mgr.pxePort)), loggedRouter)
	if err != nil {
		return mayuerror.MaskAny(err)

	}
	return nil
}

func (mgr *pxeManagerT) startAPIserver() error {
	mgr.apiRouter = mux.NewRouter()
	//  api endpoint for setting metadata of machine
	mgr.apiRouter.Methods("PUT").PathPrefix("/admin/host/{serial}/boot_complete").HandlerFunc(withSerialParam(mgr.bootComplete))
	mgr.apiRouter.Methods("PUT").PathPrefix("/admin/host/{serial}/mark_fresh").HandlerFunc(withSerialParam(mgr.markFresh))
	mgr.apiRouter.Methods("PUT").PathPrefix("/admin/host/{serial}/set_provider_id").HandlerFunc(withSerialParam(mgr.setProviderId))
	mgr.apiRouter.Methods("PUT").PathPrefix("/admin/host/{serial}/set_ipmi_addr").HandlerFunc(withSerialParam(mgr.setIPMIAddr))
	mgr.apiRouter.Methods("PUT").PathPrefix("/admin/host/{serial}/set_state").HandlerFunc(withSerialParam(mgr.setState))
	mgr.apiRouter.Methods("PUT").PathPrefix("/admin/host/{serial}/set_etcd_cluster_token").HandlerFunc(withSerialParam(mgr.setEtcdClusterToken))
	mgr.apiRouter.Methods("PUT").PathPrefix("/admin/host/{serial}/override").HandlerFunc(withSerialParam(mgr.override))

	// list all machines/hosts method
	mgr.apiRouter.Methods("GET").PathPrefix("/admin/hosts").HandlerFunc(mgr.hostsList)
	// etcd discovery
	if mgr.useInternalEtcdDiscovery {
		etcdRouter := mgr.apiRouter.PathPrefix("/etcd").Subrouter()
		mgr.defineEtcdDiscoveryRoutes(etcdRouter)
		glog.V(8).Infoln("Enabling internal etcd discovery")
	}

	// serve static file assets
	mgr.apiRouter.PathPrefix("/fileserver").Handler(http.StripPrefix("/fileserver", http.FileServer(http.Dir(mgr.fileServerPath))))

	// add welcome handler for debugging
	mgr.apiRouter.Path("/").HandlerFunc(mgr.welcomeHandler)

	// metrics endpoint
	mgr.apiRouter.Path("/metrics").Handler(promhttp.Handler())

	glogWrapper := logging.NewGlogWrapper(8)
	loggedRouter := handlers.LoggingHandler(glogWrapper, mgr.apiRouter)

	glog.V(8).Infoln(fmt.Sprintf("starting API server at %s:%d", mgr.bindAddress, mgr.apiPort))

	if mgr.noTLS {
		err := http.ListenAndServe(net.JoinHostPort(mgr.bindAddress, strconv.Itoa(mgr.apiPort)), loggedRouter)
		if err != nil {
			return mayuerror.MaskAny(err)

		}
	} else {
		err := http.ListenAndServeTLS(net.JoinHostPort(mgr.bindAddress, strconv.Itoa(mgr.apiPort)), mgr.tlsCertFile, mgr.tlsKeyFile, loggedRouter)
		if err != nil {
			return mayuerror.MaskAny(err)

		}
	}
	return nil
}

func (mgr *pxeManagerT) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mgr.apiRouter.ServeHTTP(w, r)
}

func (mgr *pxeManagerT) updateDNSmasqs() error {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()

	mgr.config.Network.StaticHosts = []hostmgr.IPMac{}
	mgr.config.Network.IgnoredHosts = []string{}

	err := mgr.DNSmasq.updateConf(mgr.config.Network)
	if err != nil {
		return err
	}
	err = mgr.DNSmasq.Restart()
	if err != nil {
		return err
	}

	return nil
}

func (mgr *pxeManagerT) Start() error {
	err := mgr.DNSmasq.Start()
	if err != nil {
		return err
	}

	err = mgr.updateDNSmasqs()
	if err != nil {
		return err
	}

	go func() {
		err := mgr.startIPXEserver()
		if err != nil {
			panic(err)
		}
	}()

	go func() {
		err := mgr.startAPIserver()
		if err != nil {
			panic(err)
		}
	}()

	select {}
}

func (mgr *pxeManagerT) getNextProfile() string {
	profileCount := mgr.cluster.GetProfileCount()

	for _, profile := range mgr.config.Profiles {
		if profileCount[profile.Name] < profile.Quantity {
			return profile.Name
		}
	}
	return ""
}

func (mgr *pxeManagerT) getNextInternalIP() net.IP {
	assignedIPs := map[string]struct{}{}
	for _, host := range mgr.cluster.GetAllHosts() {
		assignedIPs[host.InternalAddr.String()] = struct{}{}
	}

	IPisAvailable := func(ip net.IP) bool {
		_, exists := assignedIPs[ip.String()]
		return !exists
	}

	currentIP := net.ParseIP(mgr.config.Network.IPRange.Start)
	rangeEnd := net.ParseIP(mgr.config.Network.IPRange.End)

	for ; ; ipLessThanOrEqual(currentIP, rangeEnd) {
		if IPisAvailable(currentIP) {
			return currentIP
		}
		currentIP = incIP(currentIP)
	}

}

func (mgr *pxeManagerT) apiURL() string {
	scheme := "https"
	if mgr.noTLS {
		scheme = "http"
	}
	u := url.URL{Scheme: scheme, Host: net.JoinHostPort(mgr.config.Network.BindAddr, strconv.Itoa(mgr.apiPort))}
	return u.String()
}

func (mgr *pxeManagerT) pxeURL() string {
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(mgr.config.Network.BindAddr, strconv.Itoa(mgr.pxePort))}
	return u.String()
}

func (mgr *pxeManagerT) ignitionURL() string {
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(mgr.config.Network.BindAddr, strconv.Itoa(mgr.pxePort)), Path: "/ignition"}
	return u.String()
}

func (mgr *pxeManagerT) reloadConfig() {
	newConf, err := LoadConfig(mgr.configFile)
	if err != nil {
		glog.Fatalln(err)
	}
	mgr.config = &newConf
}

func httpError(w http.ResponseWriter, msg string, status int) {
	glog.Warningln(msg)
	http.Error(w, msg, status)
}
