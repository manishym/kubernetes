/*
Copyright 2018 The Kubernetes Authors.

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

package testing

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/spf13/pflag"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/kubernetes/cmd/kube-controller-manager/app"
	kubecontrollerconfig "k8s.io/kubernetes/cmd/kube-controller-manager/app/config"
	"k8s.io/kubernetes/cmd/kube-controller-manager/app/options"

	"k8s.io/klog/v2"
)

// TearDownFunc is to be called to tear down a test server.
type TearDownFunc func()

// TestServer return values supplied by kube-test-ApiServer
type TestServer struct {
	LoopbackClientConfig *restclient.Config // Rest client config using the magic token
	Options              *options.KubeControllerManagerOptions
	Config               *kubecontrollerconfig.Config
	TearDownFn           TearDownFunc // TearDown function
	TmpDir               string       // Temp Dir used, by the apiserver
}

// Logger allows t.Testing and b.Testing to be passed to StartTestServer and StartTestServerOrDie
type Logger interface {
	Errorf(format string, args ...interface{})
	Fatalf(format string, args ...interface{})
	Logf(format string, args ...interface{})
}

// StartTestServer starts a kube-controller-manager. A rest client config and a tear-down func,
// and location of the tmpdir are returned.
//
// Note: we return a tear-down func instead of a stop channel because the later will leak temporary
// 		 files that because Golang testing's call to os.Exit will not give a stop channel go routine
// 		 enough time to remove temporary files.
func StartTestServer(t Logger, customFlags []string) (result TestServer, err error) {
	stopCh := make(chan struct{})
	var errCh chan error
	tearDown := func() {
		close(stopCh)

		// If the kube-controller-manager was started, let's wait for
		// it to shutdown clearly.
		if errCh != nil {
			err, ok := <-errCh
			if ok && err != nil {
				klog.Errorf("Failed to shutdown test server clearly: %v", err)
			}
		}
		if len(result.TmpDir) != 0 {
			os.RemoveAll(result.TmpDir)
		}
	}
	defer func() {
		if result.TearDownFn == nil {
			tearDown()
		}
	}()

	result.TmpDir, err = os.MkdirTemp("", "kube-controller-manager")
	if err != nil {
		return result, fmt.Errorf("failed to create temp dir: %v", err)
	}

	fs := pflag.NewFlagSet("test", pflag.PanicOnError)

	s, err := options.NewKubeControllerManagerOptions()
	if err != nil {
		return TestServer{}, err
	}
	all, disabled := app.KnownControllers(), app.ControllersDisabledByDefault.List()
	namedFlagSets := s.Flags(all, disabled)
	for _, f := range namedFlagSets.FlagSets {
		fs.AddFlagSet(f)
	}
	fs.Parse(customFlags)

	if s.SecureServing.BindPort != 0 {
		s.SecureServing.Listener, s.SecureServing.BindPort, err = createListenerOnFreePort()
		if err != nil {
			return result, fmt.Errorf("failed to create listener: %v", err)
		}
		s.SecureServing.ServerCert.CertDirectory = result.TmpDir

		t.Logf("kube-controller-manager will listen securely on port %d...", s.SecureServing.BindPort)
	}

	config, err := s.Config(all, disabled)
	if err != nil {
		return result, fmt.Errorf("failed to create config from options: %v", err)
	}

	errCh = make(chan error)
	go func(stopCh <-chan struct{}) {
		defer close(errCh)

		if err := app.Run(config.Complete(), stopCh); err != nil {
			errCh <- err
		}
	}(stopCh)

	t.Logf("Waiting for /healthz to be ok...")
	client, err := kubernetes.NewForConfig(config.LoopbackClientConfig)
	if err != nil {
		return result, fmt.Errorf("failed to create a client: %v", err)
	}
	err = wait.Poll(100*time.Millisecond, 30*time.Second, func() (bool, error) {
		select {
		case err := <-errCh:
			return false, err
		default:
		}

		result := client.CoreV1().RESTClient().Get().AbsPath("/healthz").Do(context.TODO())
		status := 0
		result.StatusCode(&status)
		if status == 200 {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return result, fmt.Errorf("failed to wait for /healthz to return ok: %v", err)
	}

	// from here the caller must call tearDown
	result.LoopbackClientConfig = config.LoopbackClientConfig
	result.Options = s
	result.Config = config
	result.TearDownFn = tearDown

	return result, nil
}

// StartTestServerOrDie calls StartTestServer t.Fatal if it does not succeed.
func StartTestServerOrDie(t Logger, flags []string) *TestServer {
	result, err := StartTestServer(t, flags)
	if err == nil {
		return &result
	}

	t.Fatalf("failed to launch server: %v", err)
	return nil
}

func createListenerOnFreePort() (net.Listener, int, error) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, 0, err
	}

	// get port
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		ln.Close()
		return nil, 0, fmt.Errorf("invalid listen address: %q", ln.Addr().String())
	}

	return ln, tcpAddr.Port, nil
}
