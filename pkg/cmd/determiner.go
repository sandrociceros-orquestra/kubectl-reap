package cmd

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
)

const (
	kindConfigMap             = "ConfigMap"
	kindSecret                = "Secret"
	kindPod                   = "Pod"
	kindPersistentVolumeClaim = "PersistentVolumeClaim"
	kindPodDisruptionBudget   = "PodDisruptionBudget"
)

// determiner determines whether a resource should be pruned.
type determiner struct {
	usedConfigMaps             map[string]struct{} // key=ConfigMap.Name
	usedSecrets                map[string]struct{} // key=Secret.Name
	usedPersistentVolumeClaims map[string]struct{} // key=PersistentVolumeClaim.Name

	pods []*corev1.Pod
}

func newDeterminer(clientset *kubernetes.Clientset, r *resource.Result, namespace string) (*determiner, error) {
	var (
		pruneConfigMaps             bool
		pruneSecrets                bool
		prunePersistentVolumeClaims bool
		prunePodDisruptionBudgets   bool
	)

	if err := r.Visit(func(info *resource.Info, err error) error {
		switch info.Object.GetObjectKind().GroupVersionKind().Kind {
		case kindConfigMap:
			pruneConfigMaps = true
		case kindSecret:
			pruneSecrets = true
		case kindPersistentVolumeClaim:
			prunePersistentVolumeClaims = true
		case kindPodDisruptionBudget:
			prunePodDisruptionBudgets = true
		}
		return nil
	}); err != nil {
		return nil, err
	}

	d := &determiner{}

	ctx := context.Background()

	if pruneConfigMaps || pruneSecrets || prunePersistentVolumeClaims || prunePodDisruptionBudgets {
		var err error
		d.pods, err = listPods(ctx, clientset, namespace)
		if err != nil {
			return nil, err
		}
	}

	if pruneConfigMaps {
		d.usedConfigMaps = detectUsedConfigMaps(d.pods)
	}

	if pruneSecrets {
		sas, err := listServiceAccounts(ctx, clientset, namespace)
		if err != nil {
			return nil, err
		}
		d.usedSecrets = detectUsedSecrets(d.pods, sas)
	}

	if prunePersistentVolumeClaims {
		d.usedPersistentVolumeClaims = detectUsedPersistentVolumeClaims(d.pods)
	}

	return d, nil
}

// determinePrune determines whether a resource should be pruned.
func (d *determiner) determinePrune(info *resource.Info) (bool, error) {
	switch kind := info.Object.GetObjectKind().GroupVersionKind().Kind; kind {
	case kindConfigMap:
		if _, ok := d.usedConfigMaps[info.Name]; !ok {
			return true, nil
		}

	case kindSecret:
		if _, ok := d.usedSecrets[info.Name]; !ok {
			return true, nil
		}

	case kindPersistentVolumeClaim:
		if _, ok := d.usedPersistentVolumeClaims[info.Name]; !ok {
			return true, nil
		}

	case kindPod:
		pod, err := infoToPod(info)
		if err != nil {
			return false, err
		}

		if pod.Status.Phase != corev1.PodRunning {
			return true, nil
		}

	case kindPodDisruptionBudget:
		pdb, err := infoToPodDisruptionBudget(info)
		if err != nil {
			return false, err
		}

		used, err := d.determineUsedPodDisruptionBudget(pdb)
		if err != nil {
			return false, err
		}
		return !used, nil

	default:
		return false, fmt.Errorf("unsupported kind: %s/%s", kind, info.Name)
	}

	return false, nil
}

func detectUsedConfigMaps(pods []*corev1.Pod) map[string]struct{} {
	usedConfigMaps := make(map[string]struct{})

	for _, pod := range pods {
		for _, container := range pod.Spec.Containers {
			for _, envFrom := range container.EnvFrom {
				if envFrom.ConfigMapRef != nil {
					usedConfigMaps[envFrom.ConfigMapRef.Name] = struct{}{}
				}
			}

			for _, env := range container.Env {
				if env.ValueFrom != nil && env.ValueFrom.ConfigMapKeyRef != nil {
					usedConfigMaps[env.ValueFrom.ConfigMapKeyRef.Name] = struct{}{}
				}
			}
		}

		for _, volume := range pod.Spec.Volumes {
			if volume.ConfigMap != nil {
				usedConfigMaps[volume.ConfigMap.Name] = struct{}{}
			}

			if volume.Projected != nil {
				for _, source := range volume.Projected.Sources {
					if source.ConfigMap != nil {
						usedConfigMaps[source.ConfigMap.Name] = struct{}{}
					}
				}
			}
		}
	}

	return usedConfigMaps
}

func detectUsedSecrets(pods []*corev1.Pod, sas []*corev1.ServiceAccount) map[string]struct{} {
	usedSecrets := make(map[string]struct{})

	// Add Secrets used in Pods
	for _, pod := range pods {
		for _, container := range pod.Spec.Containers {
			for _, envFrom := range container.EnvFrom {
				if envFrom.SecretRef != nil {
					usedSecrets[envFrom.SecretRef.Name] = struct{}{}
				}
			}

			for _, env := range container.Env {
				if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
					usedSecrets[env.ValueFrom.SecretKeyRef.Name] = struct{}{}
				}
			}
		}

		for _, volume := range pod.Spec.Volumes {
			if volume.Secret != nil {
				usedSecrets[volume.Secret.SecretName] = struct{}{}
			}

			if volume.Projected != nil {
				for _, source := range volume.Projected.Sources {
					if source.Secret != nil {
						usedSecrets[source.Secret.Name] = struct{}{}
					}
				}
			}
		}
	}

	// Add Secrets used in ServiceAccounts
	for _, sa := range sas {
		for _, secret := range sa.Secrets {
			usedSecrets[secret.Name] = struct{}{}
		}
	}

	return usedSecrets
}

func detectUsedPersistentVolumeClaims(pods []*corev1.Pod) map[string]struct{} {
	usedPersistentVolumeClaims := make(map[string]struct{})

	for _, pod := range pods {
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim == nil {
				continue
			}
			usedPersistentVolumeClaims[volume.PersistentVolumeClaim.ClaimName] = struct{}{}
		}
	}

	return usedPersistentVolumeClaims
}

func (d *determiner) determineUsedPodDisruptionBudget(pdb *policyv1beta1.PodDisruptionBudget) (bool, error) {
	selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
	if err != nil {
		return false, fmt.Errorf("invalid label selector (%s): %w", pdb.Name, err)
	}

	for _, pod := range d.pods {
		if selector.Matches(labels.Set(pod.Labels)) {
			return true, nil
		}
	}

	return false, nil
}
