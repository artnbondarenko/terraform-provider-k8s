package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"net/url"

	"github.com/hashicorp/terraform/helper/schema"
	"github.com/hashicorp/terraform/plugin"
	"github.com/hashicorp/terraform/terraform"
)

type config struct {
	kubeconfig        string
	kubeconfigContent string
	kubeconfigContext string
}

func main() {
	plugin.Serve(&plugin.ServeOpts{
		ProviderFunc: func() terraform.ResourceProvider {
			return &schema.Provider{
				Schema: map[string]*schema.Schema{
					"kubeconfig": &schema.Schema{
						Type:     schema.TypeString,
						Optional: true,
					},
					"kubeconfig_content": &schema.Schema{
						Type:     schema.TypeString,
						Optional: true,
					},
					"kubeconfig_context": &schema.Schema{
						Type:     schema.TypeString,
						Optional: true,
					},
				},
				ResourcesMap: map[string]*schema.Resource{
					"k8s_manifest": resourceManifest(),
				},
				ConfigureFunc: func(d *schema.ResourceData) (interface{}, error) {
					return &config{
						kubeconfig:        d.Get("kubeconfig").(string),
						kubeconfigContent: d.Get("kubeconfig_content").(string),
						kubeconfigContext: d.Get("kubeconfig_context").(string),
					}, nil
				},
			}
		},
	})
}

func resourceManifest() *schema.Resource {
	return &schema.Resource{
		Create: resourceManifestCreate,
		Read:   resourceManifestRead,
		Update: resourceManifestUpdate,
		Delete: resourceManifestDelete,

		Schema: map[string]*schema.Schema{
			"content": &schema.Schema{
				Type:      schema.TypeString,
				Required:  true,
				Sensitive: true,
			},
		},
	}
}

func run(cmd *exec.Cmd) error {
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		cmdStr := cmd.Path + " " + strings.Join(cmd.Args, " ")
		if stderr.Len() == 0 {
			return fmt.Errorf("%s: %v", cmdStr, err)
		}
		return fmt.Errorf("%s %v: %s", cmdStr, err, stderr.Bytes())
	}
	return nil
}

func kubeconfigPath(m interface{}) (string, func(), error) {
	kubeconfig := m.(*config).kubeconfig
	kubeconfigContent := m.(*config).kubeconfigContent
	var cleanupFunc = func() {}

	if kubeconfig != "" && kubeconfigContent != "" {
		return kubeconfig, cleanupFunc, fmt.Errorf("both kubeconfig and kubeconfig_content are defined, " +
			"please use only one of the paramters")
	} else if kubeconfigContent != "" {
		tmpfile, err := ioutil.TempFile("", "kubeconfig_")
		if err != nil {
			defer cleanupFunc()
			return "", cleanupFunc, fmt.Errorf("creating a kubeconfig file: %v", err)
		}

		cleanupFunc = func() { os.Remove(tmpfile.Name()) }

		if _, err = io.WriteString(tmpfile, kubeconfigContent); err != nil {
			defer cleanupFunc()
			return "", cleanupFunc, fmt.Errorf("writing kubeconfig to file: %v", err)
		}
		if err = tmpfile.Close(); err != nil {
			defer cleanupFunc()
			return "", cleanupFunc, fmt.Errorf("completion of write to kubeconfig file: %v", err)
		}

		kubeconfig = tmpfile.Name()
	}

	if kubeconfig != "" {
		return kubeconfig, cleanupFunc, nil
	}

	return "", cleanupFunc, nil
}

func kubectl(m interface{}, kubeconfig string, args ...string) *exec.Cmd {
	if kubeconfig != "" {
		args = append([]string{"--kubeconfig", kubeconfig}, args...)
	}

	context := m.(*config).kubeconfigContext
	if context != "" {
		args = append([]string{"--context", context}, args...)
	}

	return exec.Command("kubectl", args...)
}

// When packing multiple resources into a single resource ID, seperate them
// with this value.
const resourceIDSelflinkDelim = " "

func resourceManifestCreate(d *schema.ResourceData, m interface{}) error {
	kubeconfig, cleanup, err := kubeconfigPath(m)
	if err != nil {
		return fmt.Errorf("determining kubeconfig: %v", err)
	}
	defer cleanup()

	cmd := kubectl(m, kubeconfig, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(d.Get("content").(string))
	if err := run(cmd); err != nil {
		return err
	}

	stdout := &bytes.Buffer{}
	cmd = kubectl(m, kubeconfig, "get", "-f", "-", "-o", "json")
	cmd.Stdin = strings.NewReader(d.Get("content").(string))
	cmd.Stdout = stdout
	if err := run(cmd); err != nil {
		return err
	}

	var data struct {
		Items []struct {
			Metadata struct {
				Selflink string `json:"selflink"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &data); err != nil {
		return fmt.Errorf("decoding response: %v", err)
	}
	if len(data.Items) == 0 {
		return fmt.Errorf("no resources created")
	}
	var selflinks []string
	for _, item := range data.Items {
		selflink := item.Metadata.Selflink
		if selflink == "" {
			return fmt.Errorf("could not parse self-link from response %s", stdout.String())
		}
		selflinks = append(selflinks, selflink)
	}
	d.SetId(strings.Join(selflinks, resourceIDSelflinkDelim))
	return nil
}

func resourceManifestUpdate(d *schema.ResourceData, m interface{}) error {
	kubeconfig, cleanup, err := kubeconfigPath(m)
	if err != nil {
		return fmt.Errorf("determining kubeconfig: %v", err)
	}
	defer cleanup()

	cmd := kubectl(m, kubeconfig, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(d.Get("content").(string))
	return run(cmd)
}

func resourceFromSelflink(s string) (resource, namespace string, ok bool) {
	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		return "", "", false
	}
	resource = parts[len(parts)-2] + "/" + parts[len(parts)-1]

	for i, part := range parts {
		if part == "namespaces" && len(parts) > i+1 {
			namespace = parts[i+1]
			break
		}
	}

	// decode non [a-z] characters
	resource, err := url.PathUnescape(resource)
	if err != nil {
		return resource, namespace, false
	}

	return resource, namespace, true
}

type errorList []error

func (e errorList) Error() string {
	return fmt.Sprintf("%s", []error(e))
}

func resourceManifestDelete(d *schema.ResourceData, m interface{}) error {
	var errs []error
	resources := strings.Split(d.Id(), resourceIDSelflinkDelim)
	for i := len(resources)-1; i >= 0; i-- {
		if err := deleteResource(m, resources[i]); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) != 0 {
		return errorList(errs)
	}
	return nil
}

func deleteResource(m interface{}, selflink string) error {
	resource, namespace, ok := resourceFromSelflink(selflink)
	if !ok {
		return fmt.Errorf("invalid resource id: %s", selflink)
	}
	args := []string{"delete", resource}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	kubeconfig, cleanup, err := kubeconfigPath(m)
	if err != nil {
		return fmt.Errorf("determining kubeconfig: %v", err)
	}
	defer cleanup()

	cmd := kubectl(m, kubeconfig, args...)
	return run(cmd)
}

func resourceManifestRead(d *schema.ResourceData, m interface{}) error {
	var errs []error
	for _, selflink := range strings.Split(d.Id(), resourceIDSelflinkDelim) {
		if err := readResource(d, m, selflink); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) != 0 {
		return errorList(errs)
	}
	return nil
}

func readResource(d *schema.ResourceData, m interface{}, selflink string) error {
	resource, namespace, ok := resourceFromSelflink(selflink)
	if !ok {
		return fmt.Errorf("invalid resource id: %s", selflink)
	}

	args := []string{"get", "--ignore-not-found", resource}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}

	stdout := &bytes.Buffer{}
	kubeconfig, cleanup, err := kubeconfigPath(m)
	if err != nil {
		return fmt.Errorf("determining kubeconfig: %v", err)
	}
	defer cleanup()

	cmd := kubectl(m, kubeconfig, args...)
	cmd.Stdout = stdout
	if err := run(cmd); err != nil {
		return err
	}
	if strings.TrimSpace(stdout.String()) == "" {
		d.SetId("")
	}
	return nil
}
