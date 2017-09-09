/*
Copyright 2017 The Fission Authors.

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

package buildermgr

import (
	"fmt"
	"log"
	"os"
	"time"

	"k8s.io/client-go/1.5/kubernetes"
	"k8s.io/client-go/1.5/pkg/api"
	"k8s.io/client-go/1.5/pkg/api/v1"
	"k8s.io/client-go/1.5/pkg/apis/extensions/v1beta1"
	"k8s.io/client-go/1.5/pkg/labels"
	"k8s.io/client-go/1.5/pkg/util/intstr"
	"k8s.io/client-go/1.5/pkg/watch"

	"github.com/fission/fission/tpr"
)

type requestType int

const (
	GET_BUILDER requestType = iota
	CLEANUP_BUILDERS

	LABEL_ENV_NAME            = "envName"
	LABEL_ENV_RESOURCEVERSION = "envResourceVersion"
)

type (
	builderInfo struct {
		envMetadata *api.ObjectMeta
		deployment  *v1beta1.Deployment
		service     *v1.Service
	}

	request struct {
		requestType
		env      *tpr.Environment
		envList  []tpr.Environment
		respChan chan response
	}

	response struct {
		builderInfo *builderInfo
		err         error
	}

	environmentWatcher struct {
		cache                  map[string]*builderInfo
		requestChan            chan request
		builderNamespace       string
		fissionClient          *tpr.FissionClient
		kubernetesClient       *kubernetes.Clientset
		fetcherImage           string
		fetcherImagePullPolicy v1.PullPolicy
	}
)

func makeEnvironmentWatcher(fissionClient *tpr.FissionClient,
	kubernetesClient *kubernetes.Clientset, builderNamespace string) *environmentWatcher {

	fetcherImage := os.Getenv("FETCHER_IMAGE")
	if len(fetcherImage) == 0 {
		fetcherImage = "fission/fetcher"
	}

	fetcherImagePullPolicyS := os.Getenv("FETCHER_IMAGE_PULL_POLICY")
	if len(fetcherImagePullPolicyS) == 0 {
		fetcherImagePullPolicyS = "IfNotPresent"
	}

	var pullPolicy v1.PullPolicy
	switch fetcherImagePullPolicyS {
	case "Always":
		pullPolicy = v1.PullAlways
	case "Never":
		pullPolicy = v1.PullNever
	default:
		pullPolicy = v1.PullIfNotPresent
	}

	envWatcher := &environmentWatcher{
		cache:                  make(map[string]*builderInfo),
		requestChan:            make(chan request),
		builderNamespace:       builderNamespace,
		fissionClient:          fissionClient,
		kubernetesClient:       kubernetesClient,
		fetcherImage:           fetcherImage,
		fetcherImagePullPolicy: pullPolicy,
	}

	go envWatcher.service()

	return envWatcher
}

func (envw *environmentWatcher) getCacheKey(envName string, envResourceVersion string) string {
	return fmt.Sprintf("%v-%v", envName, envResourceVersion)
}

func (envw *environmentWatcher) getLabels(envName string, envResourceVersion string) map[string]string {
	sel := make(map[string]string)
	sel[LABEL_ENV_NAME] = envName
	sel[LABEL_ENV_RESOURCEVERSION] = envResourceVersion
	return sel
}

func (envw *environmentWatcher) getLabelValue(labels map[string]string, key string) string {
	return labels[key]
}

func (envw *environmentWatcher) getDelOption() *api.DeleteOptions {
	// cascading deletion
	// https://kubernetes.io/docs/concepts/workloads/controllers/garbage-collection/
	falseVal := false
	return &api.DeleteOptions{
		OrphanDependents: &falseVal,
	}
}

func (envw *environmentWatcher) watchEnvironments() {
	envw.sync()
	rv := ""
	for {
		wi, err := envw.fissionClient.Environments(api.NamespaceAll).Watch(api.ListOptions{
			ResourceVersion: rv,
		})
		if err != nil {
			log.Fatalf("Error watching environment list: %v", err)
		}

		for {
			ev, more := <-wi.ResultChan()
			if !more {
				// restart watch from last rv
				break
			}
			if ev.Type == watch.Error {
				// restart watch from the start
				rv = ""
				time.Sleep(time.Second)
				break
			}
			ht := ev.Object.(*tpr.Environment)
			rv = ht.Metadata.ResourceVersion
			envw.sync()
		}
	}
}

func (envw *environmentWatcher) sync() {
	for {
		envList, err := envw.fissionClient.Environments(api.NamespaceAll).List(api.ListOptions{})
		if err != nil {
			log.Fatalf("Error syncing environment TPR resources: %v", err)
		}

		// Create environment builders for all environments
		for i := range envList.Items {
			env := envList.Items[i]
			if len(env.Spec.Builder.Image) == 0 {
				continue
			}
			_, err := envw.getEnvBuilder(&env)
			if err != nil {
				log.Printf("Error creating builder for %v: %v", env.Metadata.Name, err)
			}
		}

		envw.cleanupEnvBuilders(envList.Items)
	}
}

func (envw *environmentWatcher) service() {
	for {
		req := <-envw.requestChan
		switch req.requestType {
		case GET_BUILDER:
			key := envw.getCacheKey(req.env.Metadata.Name, req.env.Metadata.ResourceVersion)
			builderInfo, ok := envw.cache[key]
			if !ok {
				builderInfo, err := envw.createBuilder(req.env)
				if err != nil {
					req.respChan <- response{err: err}
					continue
				}
				envw.cache[key] = builderInfo
			}
			req.respChan <- response{builderInfo: builderInfo}

		case CLEANUP_BUILDERS:
			latestEnvList := make(map[string]*tpr.Environment)
			for i := range req.envList {
				env := req.envList[i]
				key := envw.getCacheKey(env.Metadata.Name, env.Metadata.ResourceVersion)
				latestEnvList[key] = &env
			}

			// If an environment is deleted when builder manager down,
			// the builder belongs to the environment will be out-of-
			// control (an orphan builder) since there is no record in
			// cache and TPR. We need to iterate over the services &
			// deployments to remove both normal and orphan builders.

			svcList, err := envw.getBuilderServiceList(nil)
			if err != nil {
				log.Println(err.Error())
			}
			for _, svc := range svcList {
				envName := envw.getLabelValue(svc.ObjectMeta.Labels, LABEL_ENV_NAME)
				envResourceVersion := envw.getLabelValue(svc.ObjectMeta.Labels, LABEL_ENV_RESOURCEVERSION)
				key := envw.getCacheKey(envName, envResourceVersion)
				if _, ok := latestEnvList[key]; !ok {
					err := envw.deleteBuilderService(svc.ObjectMeta.Labels)
					if err != nil {
						log.Printf("Error removing builder service: %v", err)
					}
				}
				delete(envw.cache, svc.ObjectMeta.Name)
			}

			deployList, err := envw.getBuilderDeploymentList(nil)
			if err != nil {
				log.Printf(err.Error())
			}
			for _, deploy := range deployList {
				envName := envw.getLabelValue(deploy.ObjectMeta.Labels, LABEL_ENV_NAME)
				envResourceVersion := envw.getLabelValue(deploy.ObjectMeta.Labels, LABEL_ENV_RESOURCEVERSION)
				key := envw.getCacheKey(envName, envResourceVersion)
				if _, ok := latestEnvList[key]; !ok {
					err := envw.deleteBuilderDeployment(deploy.ObjectMeta.Labels)
					if err != nil {
						log.Printf("Error removing builder deployment: %v", err)
					}
				}
				delete(envw.cache, deploy.ObjectMeta.Name)
			}
		}
	}
}

func (envw *environmentWatcher) getEnvBuilder(env *tpr.Environment) (*builderInfo, error) {
	respChan := make(chan response)
	envw.requestChan <- request{
		requestType: GET_BUILDER,
		env:         env,
		respChan:    respChan,
	}
	resp := <-respChan
	return resp.builderInfo, resp.err
}

func (envw *environmentWatcher) cleanupEnvBuilders(envs []tpr.Environment) {
	envw.requestChan <- request{
		requestType: CLEANUP_BUILDERS,
		envList:     envs,
	}
}

func (envw *environmentWatcher) createBuilder(env *tpr.Environment) (*builderInfo, error) {
	var svc *v1.Service
	var deploy *v1beta1.Deployment

	sel := envw.getLabels(env.Metadata.Name, env.Metadata.ResourceVersion)

	svcList, err := envw.getBuilderServiceList(sel)
	if err != nil {
		return nil, err
	}
	if len(svcList) == 0 {
		svc, err = envw.createBuilderService(env)
		if err != nil {
			return nil, fmt.Errorf("Error creating builder service: %v", err)
		}
	}

	deployList, err := envw.getBuilderDeploymentList(sel)
	if err != nil {
		return nil, err
	}
	if len(deployList) == 0 {
		deploy, err = envw.createBuilderDeployment(env)
		if err != nil {
			return nil, fmt.Errorf("Error creating builder deployment: %v", err)
		}
	}

	return &builderInfo{
		envMetadata: &env.Metadata,
		service:     svc,
		deployment:  deploy,
	}, nil
}

func (envw *environmentWatcher) deleteBuilderService(sel map[string]string) error {
	svcList, err := envw.getBuilderServiceList(sel)
	if err != nil {
		return err
	}
	for _, svc := range svcList {
		log.Printf("Removing builder service: %v", svc.ObjectMeta.Name)
		err = envw.kubernetesClient.
			Services(envw.builderNamespace).
			Delete(svc.ObjectMeta.Name, envw.getDelOption())
		if err != nil {
			return fmt.Errorf("Error deleting builder service: %v", err)
		}
	}
	return nil
}

func (envw *environmentWatcher) deleteBuilderDeployment(sel map[string]string) error {
	deployList, err := envw.getBuilderDeploymentList(sel)
	if err != nil {
		return err
	}
	for _, deploy := range deployList {
		log.Printf("Removing builder deployment: %v", deploy.ObjectMeta.Name)
		err = envw.kubernetesClient.
			Deployments(envw.builderNamespace).
			Delete(deploy.ObjectMeta.Name, envw.getDelOption())
		if err != nil {
			return fmt.Errorf("Error deleteing builder deployment: %v", err)
		}
	}
	return nil
}

func (envw *environmentWatcher) getBuilderServiceList(sel map[string]string) ([]v1.Service, error) {
	svcList, err := envw.kubernetesClient.Services(envw.builderNamespace).List(
		api.ListOptions{
			LabelSelector: labels.Set(sel).AsSelector(),
		})
	if err != nil {
		return nil, fmt.Errorf("Error getting builder service list: %v", err)
	}
	return svcList.Items, nil
}

func (envw *environmentWatcher) createBuilderService(env *tpr.Environment) (*v1.Service, error) {
	name := envw.getCacheKey(env.Metadata.Name, env.Metadata.ResourceVersion)
	sel := envw.getLabels(env.Metadata.Name, env.Metadata.ResourceVersion)
	service := v1.Service{
		ObjectMeta: v1.ObjectMeta{
			Namespace: envw.builderNamespace,
			Name:      name,
			Labels:    sel,
		},
		Spec: v1.ServiceSpec{
			Selector: sel,
			Type:     v1.ServiceTypeClusterIP,
			Ports: []v1.ServicePort{
				{
					Name:     "fetcher-port",
					Protocol: v1.ProtocolTCP,
					Port:     8000,
					TargetPort: intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: 8000,
					},
				},
				{
					Name:     "builder-port",
					Protocol: v1.ProtocolTCP,
					Port:     8001,
					TargetPort: intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: 8001,
					},
				},
			},
		},
	}
	log.Printf("Creating builder service: %v", name)
	_, err := envw.kubernetesClient.Services(envw.builderNamespace).Create(&service)
	if err != nil {
		return nil, err
	}
	return &service, nil
}

func (envw *environmentWatcher) getBuilderDeploymentList(sel map[string]string) ([]v1beta1.Deployment, error) {
	deployList, err := envw.kubernetesClient.Deployments(envw.builderNamespace).List(
		api.ListOptions{
			LabelSelector: labels.Set(sel).AsSelector(),
		})
	if err != nil {
		return nil, fmt.Errorf("Error getting builder deployment list: %v", err)
	}
	return deployList.Items, nil
}

func (envw *environmentWatcher) createBuilderDeployment(env *tpr.Environment) (*v1beta1.Deployment, error) {
	sharedMountPath := "/package"
	name := envw.getCacheKey(env.Metadata.Name, env.Metadata.ResourceVersion)
	sel := envw.getLabels(env.Metadata.Name, env.Metadata.ResourceVersion)
	var replicas int32 = 1
	deployment := &v1beta1.Deployment{
		ObjectMeta: v1.ObjectMeta{
			Namespace: envw.builderNamespace,
			Name:      name,
			Labels:    sel,
		},
		Spec: v1beta1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &v1beta1.LabelSelector{
				MatchLabels: sel,
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: v1.ObjectMeta{
					Labels: sel,
				},
				Spec: v1.PodSpec{
					Volumes: []v1.Volume{
						{
							Name: "package",
							VolumeSource: v1.VolumeSource{
								EmptyDir: &v1.EmptyDirVolumeSource{},
							},
						},
					},
					Containers: []v1.Container{
						{
							Name:                   "builder",
							Image:                  env.Spec.Builder.Image,
							ImagePullPolicy:        v1.PullAlways,
							TerminationMessagePath: "/dev/termination-log",
							VolumeMounts: []v1.VolumeMount{
								{
									Name:      "package",
									MountPath: sharedMountPath,
								},
							},
							Command: []string{"/builder", sharedMountPath},
						},
						{
							Name:                   "fetcher",
							Image:                  envw.fetcherImage,
							ImagePullPolicy:        envw.fetcherImagePullPolicy,
							TerminationMessagePath: "/dev/termination-log",
							VolumeMounts: []v1.VolumeMount{
								{
									Name:      "package",
									MountPath: sharedMountPath,
								},
							},
							Command: []string{"/fetcher", sharedMountPath},
						},
					},
					ServiceAccountName: "fission-builder",
				},
			},
		},
	}
	log.Printf("Creating builder deployment: %v", envw.getCacheKey(env.Metadata.Name, env.Metadata.ResourceVersion))
	_, err := envw.kubernetesClient.Deployments(envw.builderNamespace).Create(deployment)
	if err != nil {
		return nil, err
	}
	return deployment, nil
}
