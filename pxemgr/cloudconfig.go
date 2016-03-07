package pxemgr

import (
	"io"
	"io/ioutil"
	"os"
	"path"
	"text/template"

	"github.com/giantswarm/mayu/hostmgr"
	"github.com/golang/glog"
)

var snippetsFiles []string

func maybeInitSnippets(snippets string) {
	if snippetsFiles != nil {
		return
	}
	snippetsFiles = []string{}

	if len(snippets) > 0 {
		if _, err := os.Stat(snippets); err == nil {
			if fis, err := ioutil.ReadDir(snippets); err == nil {
				for _, fi := range fis {
					snippetsFiles = append(snippetsFiles, path.Join(snippets, fi.Name()))
				}
			}
		}
	}
}

func getTemplate(path, snippets string) (*template.Template, error) {
	maybeInitSnippets(snippets)
	templates := []string{path}
	templates = append(templates, snippetsFiles...)
	glog.V(10).Infof("templates: %+v\n", templates)

	return template.ParseFiles(templates...)
}

func (mgr *pxeManagerT) WriteLastStageCC(host hostmgr.Host, wr io.Writer) error {
	ctx := struct {
		Host             hostmgr.Host
		EtcdDiscoveryUrl string
		ClusterNetwork   network
		MayuHost         string
		MayuPort         int
		MayuURL          string
		PostBootURL      string
		NoSecure         bool
		TemplatesEnv     map[string]interface{}
	}{
		Host:             host,
		ClusterNetwork:   mgr.config.Network,
		EtcdDiscoveryUrl: mgr.cluster.Config.EtcdDiscoveryURL,
		MayuHost:         mgr.config.Network.BindAddr,
		MayuPort:         mgr.httpPort,
		MayuURL:          mgr.thisHost(),
		PostBootURL:      mgr.thisHost() + "/admin/host/" + host.Serial + "/boot_complete",
		NoSecure:         mgr.noSecure,
		TemplatesEnv:     mgr.config.TemplatesEnv,
	}

	tmpl, err := getTemplate(mgr.lastStageCloudconfig, mgr.templateSnippets)
	if err != nil {
		glog.Fatalln(err)
		return err
	}

	return tmpl.Execute(wr, ctx)
}
