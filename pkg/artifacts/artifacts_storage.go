/*
Copyright 2018 The Knative Authors.

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

package artifacts

import (
	"fmt"
	"strings"

	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1alpha1"
	"github.com/tektoncd/pipeline/pkg/system"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ArtifactStorageInterface is an interface to define the steps to copy
// an pipeline artifact to/from temporary storage
type ArtifactStorageInterface interface {
	GetCopyToStorageFromContainerSpec(name, sourcePath, destinationPath string) []corev1.Container
	GetCopyFromStorageToContainerSpec(name, sourcePath, destinationPath string) []corev1.Container
	GetSecretsVolumes() []corev1.Volume
	GetType() string
	StorageBasePath(pr *v1alpha1.PipelineRun) string
}

// InitializeArtifactStorage will check if there is there is a
// bucket configured or create a PVC
func InitializeArtifactStorage(pr *v1alpha1.PipelineRun, c kubernetes.Interface, logger *zap.SugaredLogger) (ArtifactStorageInterface, error) {
	configMap, err := c.CoreV1().ConfigMaps(system.GetNamespace()).Get(v1alpha1.BucketConfigName, metav1.GetOptions{})
	shouldCreatePVC, err := NeedsPVC(configMap, err, logger)
	if err != nil {
		return nil, err
	}
	if shouldCreatePVC {
		err = createPVC(pr, c)
		if err != nil {
			return nil, err
		}
		return &v1alpha1.ArtifactPVC{Name: pr.Name}, nil
	}

	return NewArtifactBucketConfigFromConfigMap(configMap)
}

// CleanupArtifactStorage will delete the PipelineRun's artifact storage PVC if it exists. The PVC is created for using
// an output workspace or artifacts from one Task to another Task. No other PVCs will be impacted by this cleanup.
func CleanupArtifactStorage(pr *v1alpha1.PipelineRun, c kubernetes.Interface, logger *zap.SugaredLogger) error {
	configMap, err := c.CoreV1().ConfigMaps(system.GetNamespace()).Get(v1alpha1.BucketConfigName, metav1.GetOptions{})
	shouldCreatePVC, err := NeedsPVC(configMap, err, logger)
	if err != nil {
		return err
	}
	if shouldCreatePVC {
		err = deletePVC(pr, c)
		if err != nil {
			return err
		}
	}
	return nil
}

// NeedsPVC checks if the possibly-nil config map passed to it is configured to use a bucket for artifact storage,
// returning true if instead a PVC is needed.
func NeedsPVC(configMap *corev1.ConfigMap, err error, logger *zap.SugaredLogger) (bool, error) {
	if err != nil {
		if errors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("couldn't determine if PVC was needed from config map: %s", err)
	}
	if configMap == nil {
		return true, nil
	}
	if configMap.Data == nil {
		logger.Warn("the configmap has no data")
		return true, nil
	}
	if location, ok := configMap.Data[v1alpha1.BucketLocationKey]; !ok {
		return true, nil
	} else {
		logger.Warnf("the configmap key %q is empty", v1alpha1.BucketLocationKey)
		if strings.TrimSpace(location) == "" {
			return true, nil
		}
	}
	return false, nil
}

// GetArtifactStorage returns the storage interface to enable
// consumer code to get a container step for copy to/from storage
func GetArtifactStorage(prName string, c kubernetes.Interface, logger *zap.SugaredLogger) (ArtifactStorageInterface, error) {
	configMap, err := c.CoreV1().ConfigMaps(system.GetNamespace()).Get(v1alpha1.BucketConfigName, metav1.GetOptions{})
	pvc, err := NeedsPVC(configMap, err, logger)
	if err != nil {
		return nil, fmt.Errorf("couldn't determine if PVC was needed from config map: %s", err)
	}
	if pvc {
		return &v1alpha1.ArtifactPVC{Name: prName}, nil
	}
	return NewArtifactBucketConfigFromConfigMap(configMap)
}

// NewArtifactBucketConfigFromConfigMap creates a Bucket from the supplied ConfigMap
func NewArtifactBucketConfigFromConfigMap(configMap *corev1.ConfigMap) (*v1alpha1.ArtifactBucket, error) {
	c := &v1alpha1.ArtifactBucket{}

	if configMap.Data == nil {
		return c, nil
	}
	if location, ok := configMap.Data[v1alpha1.BucketLocationKey]; !ok {
		c.Location = ""
	} else {
		c.Location = location
	}
	sp := v1alpha1.SecretParam{}
	if secretName, ok := configMap.Data[v1alpha1.BucketServiceAccountSecretName]; ok {
		if secretKey, ok := configMap.Data[v1alpha1.BucketServiceAccountSecretKey]; ok {
			sp.FieldName = "GOOGLE_APPLICATION_CREDENTIALS"
			sp.SecretName = secretName
			sp.SecretKey = secretKey
			c.Secrets = append(c.Secrets, sp)
		}
	}
	return c, nil
}

func createPVC(pr *v1alpha1.PipelineRun, c kubernetes.Interface) error {
	if _, err := c.CoreV1().PersistentVolumeClaims(pr.Namespace).Get(GetPVCName(pr), metav1.GetOptions{}); err != nil {
		if errors.IsNotFound(err) {
			pvc := GetPVCSpec(pr)
			if _, err := c.CoreV1().PersistentVolumeClaims(pr.Namespace).Create(pvc); err != nil {
				return fmt.Errorf("failed to claim Persistent Volume %q due to error: %s", pr.Name, err)
			}
			return nil
		}
		return fmt.Errorf("failed to get claim Persistent Volume %q due to error: %s", pr.Name, err)
	}
	return nil
}

func deletePVC(pr *v1alpha1.PipelineRun, c kubernetes.Interface) error {
	if _, err := c.CoreV1().PersistentVolumeClaims(pr.Namespace).Get(GetPVCName(pr), metav1.GetOptions{}); err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to get Persistent Volume %q due to error: %s", GetPVCName(pr), err)
		}
	} else if err := c.CoreV1().PersistentVolumeClaims(pr.Namespace).Delete(GetPVCName(pr), &metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("failed to delete Persistent Volume %q due to error: %s", pr.Name, err)
	}
	return nil
}

// GetPVCSpec returns the PVC to create for a given PipelineRun
func GetPVCSpec(pr *v1alpha1.PipelineRun) *corev1.PersistentVolumeClaim {
	// TODO(shashwathi): make this value configurable
	pvcSizeBytes := int64(5 * 1024 * 1024 * 1024) // 5 GBs
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       pr.Namespace,
			Name:            GetPVCName(pr),
			OwnerReferences: pr.GetOwnerReference(),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.ResourceRequirements{
				Requests: map[corev1.ResourceName]resource.Quantity{
					corev1.ResourceStorage: *resource.NewQuantity(pvcSizeBytes, resource.BinarySI),
				},
			},
		},
	}
}

// GetPVCName returns the name that should be used for the PVC for a PipelineRun
func GetPVCName(pr *v1alpha1.PipelineRun) string {
	return fmt.Sprintf("%s-pvc", pr.Name)
}
