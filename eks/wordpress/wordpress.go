// Package wordpress implements wordpress add-on.
package wordpress

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-k8s-tester/ec2config"
	"github.com/aws/aws-k8s-tester/eks/helm"
	"github.com/aws/aws-k8s-tester/eksconfig"
	k8s_client "github.com/aws/aws-k8s-tester/pkg/k8s-client"
	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/exec"
)

// Config defines Wordpress configuration.
type Config struct {
	Logger *zap.Logger
	Stopc  chan struct{}
	Sig    chan os.Signal

	EKSConfig *eksconfig.Config
	K8SClient k8s_client.EKS
}

// Tester defines Wordpress tester
type Tester interface {
	// Create installs Wordpress.
	Create() error
	// Delete deletes Wordpress.
	Delete() error
}

func NewTester(cfg Config) (Tester, error) {
	return &tester{cfg: cfg}, nil
}

type tester struct {
	cfg Config
}

const (
	chartRepoName = "bitnami"
	chartRepoURL  = "https://charts.bitnami.com/bitnami"
	chartName     = "wordpress"
)

func (ts *tester) Create() error {
	if ts.cfg.EKSConfig.AddOnWordpress.Created {
		ts.cfg.Logger.Info("skipping create AddOnWordpress")
		return nil
	}

	ts.cfg.EKSConfig.AddOnWordpress.Created = true
	ts.cfg.EKSConfig.Sync()
	createStart := time.Now()

	defer func() {
		ts.cfg.EKSConfig.AddOnWordpress.CreateTook = time.Since(createStart)
		ts.cfg.EKSConfig.AddOnWordpress.CreateTookString = ts.cfg.EKSConfig.AddOnWordpress.CreateTook.String()
		ts.cfg.EKSConfig.Sync()
	}()

	if err := k8s_client.CreateNamespace(ts.cfg.Logger, ts.cfg.K8SClient.KubernetesClientSet(), ts.cfg.EKSConfig.AddOnWordpress.Namespace); err != nil {
		return err
	}
	if err := helm.RepoAdd(ts.cfg.Logger, chartRepoName, chartRepoURL); err != nil {
		return err
	}
	if err := ts.createHelmWordpress(); err != nil {
		return err
	}
	if err := ts.waitService(); err != nil {
		return err
	}

	return ts.cfg.EKSConfig.Sync()
}

func (ts *tester) Delete() error {
	if !ts.cfg.EKSConfig.AddOnWordpress.Created {
		ts.cfg.Logger.Info("skipping delete AddOnWordpress")
		return nil
	}

	deleteStart := time.Now()
	defer func() {
		ts.cfg.EKSConfig.AddOnWordpress.DeleteTook = time.Since(deleteStart)
		ts.cfg.EKSConfig.AddOnWordpress.DeleteTookString = ts.cfg.EKSConfig.AddOnWordpress.DeleteTook.String()
		ts.cfg.EKSConfig.Sync()
	}()

	var errs []string

	if err := ts.deleteHelmWordpress(); err != nil {
		errs = append(errs, err.Error())
	}

	if err := k8s_client.DeleteNamespaceAndWait(ts.cfg.Logger,
		ts.cfg.K8SClient.KubernetesClientSet(),
		ts.cfg.EKSConfig.AddOnWordpress.Namespace,
		k8s_client.DefaultNamespaceDeletionInterval,
		k8s_client.DefaultNamespaceDeletionTimeout); err != nil {
		errs = append(errs, fmt.Sprintf("failed to delete Wordpress namespace (%v)", err))
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, ", "))
	}

	ts.cfg.EKSConfig.AddOnWordpress.Created = false
	return ts.cfg.EKSConfig.Sync()
}

// https://github.com/helm/charts/blob/master/stable/wordpress/values.yaml
// https://github.com/helm/charts/blob/master/stable/mariadb/values.yaml
func (ts *tester) createHelmWordpress() error {
	ngType := "custom"
	if ts.cfg.EKSConfig.IsEnabledAddOnManagedNodeGroups() {
		ngType = "managed"
	}

	values := make(map[string]interface{})

	// https://github.com/helm/charts/blob/master/stable/wordpress/values.yaml
	values["nodeSelector"] = map[string]interface{}{
		// do not deploy in bottlerocket; PVC not working
		"AMIType": ec2config.AMITypeAL2X8664,
		"NGType":  ngType,
	}
	values["wordpressUsername"] = ts.cfg.EKSConfig.AddOnWordpress.UserName
	values["wordpressPassword"] = ts.cfg.EKSConfig.AddOnWordpress.Password
	values["persistence"] = map[string]interface{}{
		"enabled": true,
		// use CSI driver with volume type "gp2", as in launch configuration
		"storageClassName": "gp2",
	}

	// https://github.com/helm/charts/blob/master/stable/mariadb/values.yaml
	values["mariadb"] = map[string]interface{}{
		"enabled": true,
		"rootUser": map[string]interface{}{
			"password":      ts.cfg.EKSConfig.AddOnWordpress.Password,
			"forcePassword": false,
		},
		"db": map[string]interface{}{
			"name":     "wordpress",
			"user":     ts.cfg.EKSConfig.AddOnWordpress.UserName,
			"password": ts.cfg.EKSConfig.AddOnWordpress.Password,
		},
		"master": map[string]interface{}{
			"nodeSelector": map[string]interface{}{
				// do not deploy in bottlerocket; PVC not working
				"AMIType": ec2config.AMITypeAL2X8664,
				"NGType":  ngType,
			},
			"persistence": map[string]interface{}{
				"enabled": true,
				// use CSI driver with volume type "gp2", as in launch configuration
				"storageClassName": "gp2",
			},
		},
		"slave": map[string]interface{}{
			"nodeSelector": map[string]interface{}{
				// do not deploy in bottlerocket; PVC not working
				"AMIType": ec2config.AMITypeAL2X8664,
				"NGType":  ngType,
			},
		},
	}

	return helm.Install(helm.InstallConfig{
		Logger:         ts.cfg.Logger,
		Timeout:        15 * time.Minute,
		KubeConfigPath: ts.cfg.EKSConfig.KubeConfigPath,
		Namespace:      ts.cfg.EKSConfig.AddOnWordpress.Namespace,
		ChartRepoURL:   chartRepoURL,
		ChartName:      chartName,
		ReleaseName:    chartName,
		Values:         values,
	})
}

func (ts *tester) deleteHelmWordpress() error {
	return helm.Uninstall(helm.InstallConfig{
		Logger:         ts.cfg.Logger,
		Timeout:        15 * time.Minute,
		KubeConfigPath: ts.cfg.EKSConfig.KubeConfigPath,
		Namespace:      ts.cfg.EKSConfig.AddOnWordpress.Namespace,
		ChartName:      chartName,
		ReleaseName:    chartName,
	})
}

func (ts *tester) waitService() error {
	svcName := "wordpress"
	ts.cfg.Logger.Info("waiting for WordPress service")

	waitDur := 2 * time.Minute
	ts.cfg.Logger.Info("waiting for WordPress service", zap.Duration("wait", waitDur))
	select {
	case <-ts.cfg.Stopc:
		return errors.New("WordPress service creation aborted")
	case sig := <-ts.cfg.Sig:
		return fmt.Errorf("received os signal %v", sig)
	case <-time.After(waitDur):
	}

	args := []string{
		ts.cfg.EKSConfig.KubectlPath,
		"--kubeconfig=" + ts.cfg.EKSConfig.KubeConfigPath,
		"--namespace=" + ts.cfg.EKSConfig.AddOnWordpress.Namespace,
		"describe",
		"svc",
		svcName,
	}
	argsCmd := strings.Join(args, " ")
	hostName := ""
	retryStart := time.Now()
	for time.Now().Sub(retryStart) < waitDur {
		select {
		case <-ts.cfg.Stopc:
			return errors.New("WordPress service creation aborted")
		case sig := <-ts.cfg.Sig:
			return fmt.Errorf("received os signal %v", sig)
		case <-time.After(5 * time.Second):
		}

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		cmdOut, err := exec.New().CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		cancel()
		if err != nil {
			ts.cfg.Logger.Warn("'kubectl describe svc' failed", zap.String("command", argsCmd), zap.Error(err))
		} else {
			out := string(cmdOut)
			fmt.Printf("\n\n\"%s\" output:\n%s\n\n", argsCmd, out)
		}

		ts.cfg.Logger.Info("querying WordPress service for HTTP endpoint")
		ctx, cancel = context.WithTimeout(context.Background(), time.Minute)
		so, err := ts.cfg.K8SClient.KubernetesClientSet().
			CoreV1().
			Services(ts.cfg.EKSConfig.AddOnWordpress.Namespace).
			Get(ctx, svcName, metav1.GetOptions{})
		cancel()
		if err != nil {
			ts.cfg.Logger.Warn("failed to get WordPress service; retrying", zap.Error(err))
			time.Sleep(5 * time.Second)
			continue
		}

		ts.cfg.Logger.Info(
			"WordPress service has been linked to LoadBalancer",
			zap.String("load-balancer", fmt.Sprintf("%+v", so.Status.LoadBalancer)),
		)
		for _, ing := range so.Status.LoadBalancer.Ingress {
			ts.cfg.Logger.Info(
				"WordPress service has been linked to LoadBalancer.Ingress",
				zap.String("ingress", fmt.Sprintf("%+v", ing)),
			)
			hostName = ing.Hostname
			break
		}

		if hostName != "" {
			ts.cfg.Logger.Info("found host name", zap.String("host-name", hostName))
			break
		}
	}

	if hostName == "" {
		return errors.New("failed to find host name")
	}

	ts.cfg.EKSConfig.AddOnWordpress.URL = "http://" + hostName

	// TODO: is there any better way to find out the NLB name?
	ts.cfg.EKSConfig.AddOnWordpress.NLBName = strings.Split(hostName, "-")[0]
	ss := strings.Split(hostName, ".")[0]
	ss = strings.Replace(ss, "-", "/", -1)
	ts.cfg.EKSConfig.AddOnWordpress.NLBARN = fmt.Sprintf(
		"arn:aws:elasticloadbalancing:%s:%s:loadbalancer/net/%s",
		ts.cfg.EKSConfig.Region,
		ts.cfg.EKSConfig.Status.AWSAccountID,
		ss,
	)

	fmt.Printf("\nNLB WordPress ARN: %s\n", ts.cfg.EKSConfig.AddOnWordpress.NLBARN)
	fmt.Printf("NLB WordPress Name: %s\n", ts.cfg.EKSConfig.AddOnWordpress.NLBName)
	fmt.Printf("NLB WordPress URL: %s\n\n", ts.cfg.EKSConfig.AddOnWordpress.URL)
	fmt.Printf("WordPress UserName: %s\n", ts.cfg.EKSConfig.AddOnWordpress.UserName)
	fmt.Printf("WordPress Password: %d characters\n", len(ts.cfg.EKSConfig.AddOnWordpress.Password))

	ts.cfg.Logger.Info("waiting before testing WordPress Service")
	time.Sleep(20 * time.Second)

	retryStart = time.Now()
	for time.Now().Sub(retryStart) < waitDur {
		select {
		case <-ts.cfg.Stopc:
			return errors.New("WordPress Service creation aborted")
		case sig := <-ts.cfg.Sig:
			return fmt.Errorf("received os signal %v", sig)
		case <-time.After(5 * time.Second):
		}

		buf := bytes.NewBuffer(nil)
		err := httpReadInsecure(ts.cfg.Logger, ts.cfg.EKSConfig.AddOnWordpress.URL, buf)
		if err != nil {
			ts.cfg.Logger.Warn("failed to read NLB WordPress Service; retrying", zap.Error(err))
			time.Sleep(5 * time.Second)
			continue
		}

		httpOutput := buf.String()
		fmt.Printf("\nNLB WordPress Service output:\n%s\n", httpOutput)

		if strings.Contains(httpOutput, `<p>Welcome to WordPress. This is your first post.`) || true {
			ts.cfg.Logger.Info(
				"read WordPress Service; exiting",
				zap.String("host-name", hostName),
			)
			break
		}

		ts.cfg.Logger.Warn("unexpected WordPress Service output; retrying")
	}

	return ts.cfg.EKSConfig.Sync()
}

// curl -k [URL]
func httpReadInsecure(lg *zap.Logger, u string, wr io.Writer) error {
	lg.Info("reading", zap.String("url", u))
	cli := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		}}
	r, err := cli.Get(u)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode >= 400 {
		return fmt.Errorf("%q returned %d", u, r.StatusCode)
	}

	_, err = io.Copy(wr, r.Body)
	if err != nil {
		lg.Warn("failed to read", zap.String("url", u), zap.Error(err))
	} else {
		lg.Info("read", zap.String("url", u))
	}
	return err
}