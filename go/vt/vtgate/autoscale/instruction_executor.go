package autoscale

import (
	"context"
	"fmt"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"vitess.io/vitess/go/vt/log"
)

func scaleInOutStatefulSet(clientset *kubernetes.Clientset, namespace string, statefulSetName string, replicas int32) error {
	statefulSetsClient := clientset.AppsV1().StatefulSets(namespace)

	// 获取当前的 StatefulSet
	statefulSet, err := statefulSetsClient.Get(context.TODO(), statefulSetName, metav1.GetOptions{})
	if err != nil {
		log.Errorf("Error getting StatefulSet %s: %s", statefulSetName, err)
		return err
	}

	// 修改副本数
	statefulSet.Spec.Replicas = &replicas

	// 更新 StatefulSet
	_, err = statefulSetsClient.Update(context.TODO(), statefulSet, metav1.UpdateOptions{})
	if err != nil {
		log.Errorf("Error updating StatefulSet %s: %s", statefulSetName, err)
	} else {
		fmt.Printf("Successfully updated StatefulSet %s.\n", statefulSetName)
	}
	return err
}

func scaleUpDownPod(clientset *kubernetes.Clientset, namespace string, podName string,
	cpuRequest, memoryRequest, cpuLimit, memoryLimit int64) error {
	podsClient := clientset.CoreV1().Pods(namespace)

	// 获取当前的 Pod
	pod, err := podsClient.Get(context.TODO(), podName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	// 修改容器的资源请求和限制
	for i, container := range pod.Spec.Containers {
		// 假设容器名称为 "mysql"
		if container.Name == "mysql" { // todo magic string
			// 设置新的资源请求和限制
			pod.Spec.Containers[i].Resources.Requests = v1.ResourceList{
				v1.ResourceCPU:    *resource.NewMilliQuantity(cpuRequest, resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(memoryRequest, resource.BinarySI),
			}
			pod.Spec.Containers[i].Resources.Limits = v1.ResourceList{
				v1.ResourceCPU:    *resource.NewMilliQuantity(cpuLimit, resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(memoryLimit, resource.BinarySI),
			}
		}
	}

	// 更新 Pod
	_, err = podsClient.Update(context.TODO(), pod, metav1.UpdateOptions{})
	if err == nil {
		fmt.Printf("Successfully updated resources for Pod %s.\n", podName)
	}
	return err
}