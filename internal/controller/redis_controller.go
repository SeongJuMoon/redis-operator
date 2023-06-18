/*
Copyright 2023.

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

package controller

import (
	"context"
	"fmt"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	webappv1 "overconfigured.dev/redis/api/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"strings"
	"time"
)

const (
	OperatedContainerName = "redis"
)

// RedisReconciler reconciles a Redis object
type RedisReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=webapp.overconfigured.dev,resources=redis,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=webapp.overconfigured.dev,resources=redis/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=webapp.overconfigured.dev,resources=redis/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Redis object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.4/pkg/reconcile

func (r *RedisReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	reqLogger := log.FromContext(ctx).WithValues("req.Namespace", req.Namespace, "req.Name", req.Name)
	reqLogger.Info("Reconciling Redis.")

	// Fetch the Memcached instance
	redis := &webappv1.Redis{}
	err := r.Client.Get(context.TODO(), req.NamespacedName, redis)
	if err != nil {
		if errors.IsNotFound(err) {
			reqLogger.Info("could not found redis, ignore since object must delete")
			return ctrl.Result{}, nil
		}
		reqLogger.Error(err, "Failed to get redis.")
		return ctrl.Result{}, err
	}

	// 디플로이먼트가 생성되어있을 때, 상태를 업데이트한다.
	deployment := &appsv1.Deployment{}
	err = r.Client.Get(context.TODO(), types.NamespacedName{Name: redis.Name, Namespace: redis.Namespace}, deployment)
	// 에러가 발생하였거나 에러가 IsNotFound 일 때,
	if err != nil && errors.IsNotFound(err) {
		dep := r.generateRedisDeployment(redis)
		reqLogger.Info("redis deployment created", "redis namespace is", redis.Namespace, "and redis name", redis.Name)
		err = r.Client.Create(context.TODO(), dep)
		if err != nil {
			reqLogger.Error(err, "redis deployment create failed", "redis namespace is", redis.Namespace, "and redis name", redis.Name)
			return ctrl.Result{}, err
		}
		//reqLogger.Info("reconcile true success.")
		return reconcile.Result{Requeue: true}, nil

	} else if err != nil {
		reqLogger.Error(err, "Failed to get redis deployment.")
		return ctrl.Result{}, err
	}

	// 리플리카 혹은 버전 정보가 변경되었을 때 처리
	replicas := redis.Spec.Replicas
	if *deployment.Spec.Replicas != replicas {
		deployment.Spec.Replicas = &replicas
		err := r.Client.Update(context.TODO(), deployment)
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	// 상태에 기록할 내용이 있으면 이 부분을 통해서 변경되었는지 체크하고 기록한다.
	redis.Status.Health = deployment.Status.ReadyReplicas == deployment.Status.AvailableReplicas
	err = r.Client.Status().Update(context.TODO(), redis)
	if err != nil {
		reqLogger.Error(err, "Failed to update redis status.")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *RedisReconciler) generateRedisDeployment(re *webappv1.Redis) *appsv1.Deployment {

	labels := generateLabelForRedis(re.Name)
	replicas := re.Spec.Replicas
	version := re.Spec.Version

	deps := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      re.Name,
			Namespace: re.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: v1.PodSpec{Containers: []v1.Container{
					{
						Image: fmt.Sprintf("%s:%s", OperatedContainerName, version),
						Ports: []v1.ContainerPort{
							{
								ContainerPort: 6379,
								Name:          "redis",
							},
						},
					},
				}},
			},
		},
		Status: appsv1.DeploymentStatus{},
	}

	ctrl.SetControllerReference(re, deps, r.Scheme)
	return deps
}

func generateLabelForRedis(name string) map[string]string {
	return map[string]string{
		"app":           "redis",
		"controller-by": name,
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *RedisReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&webappv1.Redis{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}

func getVersionList(pods []v1.Pod) []string {
	var versions []string
	for _, pod := range pods {
		// TODO(ADD): How to error handling.
		container, _ := getOperateRedisContainer(pod.Spec.Containers)
		version := getVersionInfoFromImagesTags(container.Image)
		versions = append(versions, version)
	}
	return versions
}

func getOperateRedisContainer(containers []v1.Container) (*v1.Container, error) {
	for _, container := range containers {
		if container.Name == OperatedContainerName {
			return &container, nil
		}
	}
	return nil, nil
}

func getVersionInfoFromImagesTags(tag string) string {
	ret := strings.SplitAfter(tag, ":")
	return ret[len(ret)-1]
}
