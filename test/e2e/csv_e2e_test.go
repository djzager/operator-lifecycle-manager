package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/api/core/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	extv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/operator-framework/operator-lifecycle-manager/pkg/api/apis/operators/v1alpha1"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/api/client/clientset/versioned"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/controller/install"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/lib/operatorclient"
)

var singleInstance = int32(1)

type cleanupFunc func()

var immediateDeleteGracePeriod int64 = 0

func buildCSVCleanupFunc(t *testing.T, c operatorclient.ClientInterface, crc versioned.Interface, csv v1alpha1.ClusterServiceVersion, namespace string, deleteCRDs bool) cleanupFunc {
	return func() {
		require.NoError(t, crc.OperatorsV1alpha1().ClusterServiceVersions(namespace).Delete(csv.GetName(), &metav1.DeleteOptions{}))
		if deleteCRDs {
			for _, crd := range csv.Spec.CustomResourceDefinitions.Owned {
				buildCRDCleanupFunc(c, crd.Name)()
			}
		}

		require.NoError(t, waitForDelete(func() error {
			_, err := crc.OperatorsV1alpha1().ClusterServiceVersions(namespace).Get(csv.GetName(), metav1.GetOptions{})
			return err
		}))
	}
}

func createCSV(t *testing.T, c operatorclient.ClientInterface, crc versioned.Interface, csv v1alpha1.ClusterServiceVersion, namespace string, cleanupCRDs bool) (cleanupFunc, error) {
	csv.Kind = v1alpha1.ClusterServiceVersionKind
	csv.APIVersion = v1alpha1.SchemeGroupVersion.String()
	_, err := crc.OperatorsV1alpha1().ClusterServiceVersions(namespace).Create(&csv)
	require.NoError(t, err)
	return buildCSVCleanupFunc(t, c, crc, csv, namespace, cleanupCRDs), nil

}

func buildCRDCleanupFunc(c operatorclient.ClientInterface, crdName string) cleanupFunc {
	return func() {
		err := c.ApiextensionsV1beta1Interface().ApiextensionsV1beta1().CustomResourceDefinitions().Delete(crdName, &metav1.DeleteOptions{GracePeriodSeconds: &immediateDeleteGracePeriod})
		if err != nil {
			fmt.Println(err)
		}

		waitForDelete(func() error {
			_, err := c.ApiextensionsV1beta1Interface().ApiextensionsV1beta1().CustomResourceDefinitions().Get(crdName, metav1.GetOptions{})
			return err
		})
	}
}

func createCRD(c operatorclient.ClientInterface, crd extv1beta1.CustomResourceDefinition) (cleanupFunc, error) {
	_, err := c.ApiextensionsV1beta1Interface().ApiextensionsV1beta1().CustomResourceDefinitions().Create(&crd)
	if err != nil {
		return nil, err
	}

	return buildCRDCleanupFunc(c, crd.GetName()), nil
}

func newNginxDeployment(name string) appsv1.DeploymentSpec {
	return appsv1.DeploymentSpec{
		Selector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"app": name,
			},
		},
		Replicas: &singleInstance,
		Template: v1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"app": name,
				},
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name:  genName("nginx"),
						Image: "nginx:1.7.9",
						Ports: []v1.ContainerPort{
							{
								ContainerPort: 80,
							},
						},
					},
				},
			},
		},
	}
}

type csvConditionChecker func(csv *v1alpha1.ClusterServiceVersion) bool

var csvPendingChecker = func(csv *v1alpha1.ClusterServiceVersion) bool {
	return csv.Status.Phase == v1alpha1.CSVPhasePending
}

var csvSucceededChecker = func(csv *v1alpha1.ClusterServiceVersion) bool {
	return csv.Status.Phase == v1alpha1.CSVPhaseSucceeded
}

var csvReplacingChecker = func(csv *v1alpha1.ClusterServiceVersion) bool {
	return csv.Status.Phase == v1alpha1.CSVPhaseReplacing || csv.Status.Phase == v1alpha1.CSVPhaseDeleting
}

var csvAnyChecker = func(csv *v1alpha1.ClusterServiceVersion) bool {
	return csvPendingChecker(csv) || csvSucceededChecker(csv) || csvReplacingChecker(csv)
}

func fetchCSV(t *testing.T, c versioned.Interface, name string, checker csvConditionChecker) (*v1alpha1.ClusterServiceVersion, error) {
	var fetched *v1alpha1.ClusterServiceVersion
	var err error

	err = wait.Poll(pollInterval, pollDuration, func() (bool, error) {
		fetched, err = c.OperatorsV1alpha1().ClusterServiceVersions(testNamespace).Get(name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		t.Logf("%s (%s): %s", fetched.Status.Phase, fetched.Status.Reason, fetched.Status.Message)
		return checker(fetched), nil
	})

	return fetched, err
}

func waitForDeploymentToDelete(t *testing.T, c operatorclient.ClientInterface, name string) error {
	return wait.Poll(pollInterval, pollDuration, func() (bool, error) {
		t.Logf("waiting for deployment %s to delete", name)
		_, err := c.GetDeployment(testNamespace, name)
		if errors.IsNotFound(err) {
			t.Logf("deleted %s", name)
			return true, nil
		}
		if err != nil {
			t.Logf("err trying to delete %s: %s", name, err)
			return false, err
		}
		return false, nil
	})
}

func waitForCSVToDelete(t *testing.T, c versioned.Interface, name string) error {
	var err error

	err = wait.Poll(pollInterval, pollDuration, func() (bool, error) {
		fetched, err := c.OperatorsV1alpha1().ClusterServiceVersions(testNamespace).Get(name, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			return true, nil
		}
		t.Logf("%s (%s): %s", fetched.Status.Phase, fetched.Status.Reason, fetched.Status.Message)
		if err != nil {
			return false, err
		}
		return false, nil
	})

	return err
}

// TODO: same test but missing serviceaccount instead
func TestCreateCSVWithUnmetRequirementsCRD(t *testing.T) {
	defer cleaner.NotifyTestComplete(t, true)

	c := newKubeClient(t)
	crc := newCRClient(t)

	depName := genName("dep-")
	csv := v1alpha1.ClusterServiceVersion{
		TypeMeta: metav1.TypeMeta{
			Kind:       v1alpha1.ClusterServiceVersionKind,
			APIVersion: v1alpha1.ClusterServiceVersionAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: genName("csv"),
		},
		Spec: v1alpha1.ClusterServiceVersionSpec{
			InstallStrategy: newNginxInstallStrategy(depName, nil, nil),
			CustomResourceDefinitions: v1alpha1.CustomResourceDefinitions{
				Owned: []v1alpha1.CRDDescription{
					{
						DisplayName: "Not In Cluster",
						Description: "A CRD that is not currently in the cluster",
						Name:        "not.in.cluster.com",
						Version:     "v1alpha1",
						Kind:        "NotInCluster",
					},
				},
			},
		},
	}

	cleanupCSV, err := createCSV(t, c, crc, csv, testNamespace, false)
	require.NoError(t, err)
	defer cleanupCSV()

	_, err = fetchCSV(t, crc, csv.Name, csvPendingChecker)
	require.NoError(t, err)

	// Shouldn't create deployment
	_, err = c.GetDeployment(testNamespace, depName)
	require.Error(t, err)
}

func TestCreateCSVWithUnmetPermissionsCRD(t *testing.T) {
	defer cleaner.NotifyTestComplete(t, true)

	c := newKubeClient(t)
	crc := newCRClient(t)

	saName := genName("dep-")
	permissions := []install.StrategyDeploymentPermissions{
		{
			ServiceAccountName: saName,
			Rules: []rbacv1.PolicyRule{
				{
					Verbs:     []string{"create"},
					APIGroups: []string{""},
					Resources: []string{"deployment"},
				},
			},
		},
	}

	clusterPermissions := []install.StrategyDeploymentPermissions{
		{
			ServiceAccountName: saName,
			Rules: []rbacv1.PolicyRule{
				{
					Verbs:     []string{"get"},
					APIGroups: []string{""},
					Resources: []string{"deployment"},
				},
			},
		},
	}

	crdPlural := genName("ins")
	crdName := crdPlural + ".cluster.com"
	depName := genName("dep-")
	csv := v1alpha1.ClusterServiceVersion{
		TypeMeta: metav1.TypeMeta{
			Kind:       v1alpha1.ClusterServiceVersionKind,
			APIVersion: v1alpha1.ClusterServiceVersionAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: genName("csv"),
		},
		Spec: v1alpha1.ClusterServiceVersionSpec{
			InstallStrategy: newNginxInstallStrategy(depName, permissions, clusterPermissions),
			CustomResourceDefinitions: v1alpha1.CustomResourceDefinitions{
				Owned: []v1alpha1.CRDDescription{
					{
						Name:        crdName,
						Version:     "v1alpha1",
						Kind:        crdPlural,
						DisplayName: crdName,
						Description: crdName,
					},
				},
			},
		},
	}

	// Create dependency first (CRD)
	cleanupCRD, err := createCRD(c, extv1beta1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: crdName,
		},
		Spec: extv1beta1.CustomResourceDefinitionSpec{
			Group:   "cluster.com",
			Version: "v1alpha1",
			Names: extv1beta1.CustomResourceDefinitionNames{
				Plural:   crdPlural,
				Singular: crdPlural,
				Kind:     crdPlural,
				ListKind: "list" + crdPlural,
			},
			Scope: "Namespaced",
		},
	})
	require.NoError(t, err)
	defer cleanupCRD()

	cleanupCSV, err := createCSV(t, c, crc, csv, testNamespace, true)
	require.NoError(t, err)
	defer cleanupCSV()

	_, err = fetchCSV(t, crc, csv.Name, csvPendingChecker)
	require.NoError(t, err)

	// Shouldn't create deployment
	_, err = c.GetDeployment(testNamespace, depName)
	require.Error(t, err)

}

func TestCreateCSVWithUnmetRequirementsAPIService(t *testing.T) {
	defer cleaner.NotifyTestComplete(t, true)

	c := newKubeClient(t)
	crc := newCRClient(t)

	depName := genName("dep-")
	csv := v1alpha1.ClusterServiceVersion{
		TypeMeta: metav1.TypeMeta{
			Kind:       v1alpha1.ClusterServiceVersionKind,
			APIVersion: v1alpha1.ClusterServiceVersionAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: genName("csv"),
		},
		Spec: v1alpha1.ClusterServiceVersionSpec{
			InstallStrategy: newNginxInstallStrategy(depName, nil, nil),
			APIServiceDefinitions: v1alpha1.APIServiceDefinitions{
				Required: []v1alpha1.APIServiceDescription{
					{
						DisplayName: "Not In Cluster",
						Description: "An apiservice that is not currently in the cluster",
						Group:       "not.in.cluster.com",
						Version:     "v1alpha1",
						Kind:        "NotInCluster",
					},
				},
			},
		},
	}

	cleanupCSV, err := createCSV(t, c, crc, csv, testNamespace, false)
	require.NoError(t, err)
	defer cleanupCSV()

	_, err = fetchCSV(t, crc, csv.Name, csvPendingChecker)
	require.NoError(t, err)

	// Shouldn't create deployment
	_, err = c.GetDeployment(testNamespace, depName)
	require.Error(t, err)
}

func TestCreateCSVWithUnmetPermissionsAPIService(t *testing.T) {
	defer cleaner.NotifyTestComplete(t, true)

	c := newKubeClient(t)
	crc := newCRClient(t)

	saName := genName("dep-")
	permissions := []install.StrategyDeploymentPermissions{
		{
			ServiceAccountName: saName,
			Rules: []rbacv1.PolicyRule{
				{
					Verbs:     []string{"create"},
					APIGroups: []string{""},
					Resources: []string{"deployment"},
				},
			},
		},
	}

	clusterPermissions := []install.StrategyDeploymentPermissions{
		{
			ServiceAccountName: saName,
			Rules: []rbacv1.PolicyRule{
				{
					Verbs:     []string{"get"},
					APIGroups: []string{""},
					Resources: []string{"deployment"},
				},
			},
		},
	}

	depName := genName("dep-")
	csv := v1alpha1.ClusterServiceVersion{
		TypeMeta: metav1.TypeMeta{
			Kind:       v1alpha1.ClusterServiceVersionKind,
			APIVersion: v1alpha1.ClusterServiceVersionAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: genName("csv"),
		},
		Spec: v1alpha1.ClusterServiceVersionSpec{
			InstallStrategy: newNginxInstallStrategy(depName, permissions, clusterPermissions),
			// Cheating a little; this is an APIservice that will exist for the e2e tests
			APIServiceDefinitions: v1alpha1.APIServiceDefinitions{
				Required: []v1alpha1.APIServiceDescription{
					{
						Group:       "packages.apps.redhat.com",
						Version:     "v1alpha1",
						Kind:        "PackageManifest",
						DisplayName: "Package Manifest",
						Description: "An apiservice that exists",
					},
				},
			},
		},
	}

	cleanupCSV, err := createCSV(t, c, crc, csv, testNamespace, false)
	require.NoError(t, err)
	defer cleanupCSV()

	_, err = fetchCSV(t, crc, csv.Name, csvPendingChecker)
	require.NoError(t, err)

	// Shouldn't create deployment
	_, err = c.GetDeployment(testNamespace, depName)
	require.Error(t, err)
}

// TODO: same test but create serviceaccount instead
func TestCreateCSVRequirementsMetCRD(t *testing.T) {
	defer cleaner.NotifyTestComplete(t, true)

	c := newKubeClient(t)
	crc := newCRClient(t)

	sa := corev1.ServiceAccount{}
	sa.SetName(genName("sa-"))
	sa.SetNamespace(testNamespace)
	_, err := c.CreateServiceAccount(&sa)
	require.NoError(t, err, "could not create ServiceAccount")

	permissions := []install.StrategyDeploymentPermissions{
		{
			ServiceAccountName: sa.GetName(),
			Rules: []rbacv1.PolicyRule{
				{
					Verbs:     []string{"create"},
					APIGroups: []string{""},
					Resources: []string{"deployment"},
				},
			},
		},
	}

	clusterPermissions := []install.StrategyDeploymentPermissions{
		{
			ServiceAccountName: sa.GetName(),
			Rules: []rbacv1.PolicyRule{
				{
					Verbs:     []string{"get"},
					APIGroups: []string{""},
					Resources: []string{"deployment"},
				},
			},
		},
	}

	crdPlural := genName("ins")
	crdName := crdPlural + ".cluster.com"
	depName := genName("dep-")
	csv := v1alpha1.ClusterServiceVersion{
		TypeMeta: metav1.TypeMeta{
			Kind:       v1alpha1.ClusterServiceVersionKind,
			APIVersion: v1alpha1.ClusterServiceVersionAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: genName("csv"),
		},
		Spec: v1alpha1.ClusterServiceVersionSpec{
			InstallStrategy: newNginxInstallStrategy(depName, permissions, clusterPermissions),
			CustomResourceDefinitions: v1alpha1.CustomResourceDefinitions{
				Owned: []v1alpha1.CRDDescription{
					{
						Name:        crdName,
						Version:     "v1alpha1",
						Kind:        crdPlural,
						DisplayName: crdName,
						Description: crdName,
					},
				},
			},
		},
	}

	// Create dependency first (CRD)
	cleanupCRD, err := createCRD(c, extv1beta1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: crdName,
		},
		Spec: extv1beta1.CustomResourceDefinitionSpec{
			Group:   "cluster.com",
			Version: "v1alpha1",
			Names: extv1beta1.CustomResourceDefinitionNames{
				Plural:   crdPlural,
				Singular: crdPlural,
				Kind:     crdPlural,
				ListKind: "list" + crdPlural,
			},
			Scope: "Namespaced",
		},
	})
	require.NoError(t, err)
	defer cleanupCRD()

	// Create Role/Cluster Roles and RoleBindings
	role := rbacv1.Role{
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:     []string{"create"},
				APIGroups: []string{""},
				Resources: []string{"deployment"},
			},
		},
	}
	role.SetName(genName("dep-"))
	role.SetNamespace(testNamespace)
	_, err = c.CreateRole(&role)
	require.NoError(t, err, "could not create Role")

	roleBinding := rbacv1.RoleBinding{
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				APIGroup:  "",
				Name:      sa.GetName(),
				Namespace: sa.GetNamespace(),
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     role.GetName(),
		},
	}
	roleBinding.SetName(genName("dep-"))
	roleBinding.SetNamespace(testNamespace)
	_, err = c.CreateRoleBinding(&roleBinding)
	require.NoError(t, err, "could not create RoleBinding")

	clusterRole := rbacv1.ClusterRole{
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:     []string{"get"},
				APIGroups: []string{""},
				Resources: []string{"deployment"},
			},
		},
	}
	clusterRole.SetName(genName("dep-"))
	_, err = c.CreateClusterRole(&clusterRole)
	require.NoError(t, err, "could not create ClusterRole")

	clusterRoleBinding := rbacv1.ClusterRoleBinding{
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				APIGroup:  "",
				Name:      sa.GetName(),
				Namespace: sa.GetNamespace(),
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     clusterRole.GetName(),
		},
	}
	clusterRoleBinding.SetName(genName("dep-"))
	_, err = c.CreateClusterRoleBinding(&clusterRoleBinding)
	require.NoError(t, err, "could not create ClusterRoleBinding")

	cleanupCSV, err := createCSV(t, c, crc, csv, testNamespace, true)
	require.NoError(t, err)
	defer cleanupCSV()

	fetchedCSV, err := fetchCSV(t, crc, csv.Name, csvSucceededChecker)
	require.NoError(t, err)

	// Should create deployment
	dep, err := c.GetDeployment(testNamespace, depName)
	require.NoError(t, err)
	require.Equal(t, depName, dep.Name)

	// Fetch cluster service version again to check for unnecessary control loops
	sameCSV, err := fetchCSV(t, crc, csv.Name, csvSucceededChecker)
	require.NoError(t, err)
	compareResources(t, fetchedCSV, sameCSV)
}

func TestCreateCSVRequirementsMetAPIService(t *testing.T) {
	defer cleaner.NotifyTestComplete(t, true)

	c := newKubeClient(t)
	crc := newCRClient(t)

	sa := corev1.ServiceAccount{}
	sa.SetName(genName("sa-"))
	sa.SetNamespace(testNamespace)
	_, err := c.CreateServiceAccount(&sa)
	require.NoError(t, err, "could not create ServiceAccount")

	permissions := []install.StrategyDeploymentPermissions{
		{
			ServiceAccountName: sa.GetName(),
			Rules: []rbacv1.PolicyRule{
				{
					Verbs:     []string{"create"},
					APIGroups: []string{""},
					Resources: []string{"deployment"},
				},
			},
		},
	}

	clusterPermissions := []install.StrategyDeploymentPermissions{
		{
			ServiceAccountName: sa.GetName(),
			Rules: []rbacv1.PolicyRule{
				{
					Verbs:     []string{"get"},
					APIGroups: []string{""},
					Resources: []string{"deployment"},
				},
			},
		},
	}

	depName := genName("dep-")
	csv := v1alpha1.ClusterServiceVersion{
		TypeMeta: metav1.TypeMeta{
			Kind:       v1alpha1.ClusterServiceVersionKind,
			APIVersion: v1alpha1.ClusterServiceVersionAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: genName("csv"),
		},
		Spec: v1alpha1.ClusterServiceVersionSpec{
			InstallStrategy: newNginxInstallStrategy(depName, permissions, clusterPermissions),
			// Cheating a little; this is an APIservice that will exist for the e2e tests
			APIServiceDefinitions: v1alpha1.APIServiceDefinitions{
				Required: []v1alpha1.APIServiceDescription{
					{
						Group:       "packages.apps.redhat.com",
						Version:     "v1alpha1",
						Kind:        "PackageManifest",
						DisplayName: "Package Manifest",
						Description: "An apiservice that exists",
					},
				},
			},
		},
	}

	// Create Role/Cluster Roles and RoleBindings
	role := rbacv1.Role{
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:     []string{"create"},
				APIGroups: []string{""},
				Resources: []string{"deployment"},
			},
		},
	}
	role.SetName(genName("dep-"))
	role.SetNamespace(testNamespace)
	_, err = c.CreateRole(&role)
	require.NoError(t, err, "could not create Role")

	roleBinding := rbacv1.RoleBinding{
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				APIGroup:  "",
				Name:      sa.GetName(),
				Namespace: sa.GetNamespace(),
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     role.GetName(),
		},
	}
	roleBinding.SetName(genName("dep-"))
	roleBinding.SetNamespace(testNamespace)
	_, err = c.CreateRoleBinding(&roleBinding)
	require.NoError(t, err, "could not create RoleBinding")

	clusterRole := rbacv1.ClusterRole{
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:     []string{"get"},
				APIGroups: []string{""},
				Resources: []string{"deployment"},
			},
		},
	}
	clusterRole.SetName(genName("dep-"))
	_, err = c.CreateClusterRole(&clusterRole)
	require.NoError(t, err, "could not create ClusterRole")

	clusterRoleBinding := rbacv1.ClusterRoleBinding{
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				APIGroup:  "",
				Name:      sa.GetName(),
				Namespace: sa.GetNamespace(),
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     clusterRole.GetName(),
		},
	}
	clusterRoleBinding.SetName(genName("dep-"))
	_, err = c.CreateClusterRoleBinding(&clusterRoleBinding)
	require.NoError(t, err, "could not create ClusterRoleBinding")

	cleanupCSV, err := createCSV(t, c, crc, csv, testNamespace, true)
	require.NoError(t, err)
	defer cleanupCSV()

	fetchedCSV, err := fetchCSV(t, crc, csv.Name, csvSucceededChecker)
	require.NoError(t, err)

	// Should create deployment
	dep, err := c.GetDeployment(testNamespace, depName)
	require.NoError(t, err)
	require.Equal(t, depName, dep.Name)

	// Fetch cluster service version again to check for unnecessary control loops
	sameCSV, err := fetchCSV(t, crc, csv.Name, csvSucceededChecker)
	require.NoError(t, err)
	compareResources(t, fetchedCSV, sameCSV)
}

func TestCreateCSVWithOwnedAPIService(t *testing.T) {
	defer cleaner.NotifyTestComplete(t, true)

	c := newKubeClient(t)
	crc := newCRClient(t)

	// Cheat and use a deployment of an extension-apiserver that we know already exists
	depName := "package-server"
	dep, err := c.KubernetesInterface().AppsV1().Deployments(testNamespace).Get(depName, metav1.GetOptions{})
	require.NoError(t, err, fmt.Sprintf("deployment %s expected but not present in namespace %s", depName, testNamespace))

	// Create CSV for the package-server
	strategy := install.StrategyDetailsDeployment{
		DeploymentSpecs: []install.StrategyDeploymentSpec{
			{
				Name: depName,
				Spec: dep.Spec,
			},
		},
	}
	strategyRaw, err := json.Marshal(strategy)

	csv := v1alpha1.ClusterServiceVersion{
		Spec: v1alpha1.ClusterServiceVersionSpec{
			InstallStrategy: v1alpha1.NamedInstallStrategy{
				StrategyName:    install.InstallStrategyNameDeployment,
				StrategySpecRaw: strategyRaw,
			},
			APIServiceDefinitions: v1alpha1.APIServiceDefinitions{
				Owned: []v1alpha1.APIServiceDescription{
					{
						Group:          "packages.apps.redhat.com",
						Version:        "v1alpha1",
						Kind:           "PackageManifest",
						DeploymentName: depName,
						ContainerPort:  int32(443),
						DisplayName:    "Package Manifest",
						Description:    "An apiservice that exists",
					},
				},
			},
		},
	}
	csv.SetName(depName)

	// Delete expected ClusterRoleBinding to system:auth-delegator
	err = c.KubernetesInterface().RbacV1().ClusterRoleBindings().Delete("packagemanifest:system:auth-delegator", &metav1.DeleteOptions{})
	require.NoError(t, waitForDelete(func() error {
		_, err := c.KubernetesInterface().RbacV1().ClusterRoleBindings().Get("packagemanifest:system:auth-delegator", metav1.GetOptions{})
		return err
	}), "could not delete expected ClusterRoleBinding before creating CSV")

	// Delete expected RoleBinding to extension-apiserver-authentication-reader
	err = c.KubernetesInterface().RbacV1().RoleBindings("kube-system").Delete("packagemanifest-auth-reader", &metav1.DeleteOptions{})
	require.NoError(t, waitForDelete(func() error {
		_, err := c.KubernetesInterface().RbacV1().RoleBindings("kube-system").Get("packagemanifest-auth-reader", metav1.GetOptions{})
		return err
	}), "could not delete expected ClusterRoleBinding before creating CSV")

	_, err = createCSV(t, c, crc, csv, testNamespace, false)
	require.NoError(t, err)

	fetchedCSV, err := fetchCSV(t, crc, csv.Name, csvSucceededChecker)
	require.NoError(t, err)

	// Should create deployment
	dep, err = c.GetDeployment(testNamespace, depName)
	require.NoError(t, err)

	// Fetch cluster service version again to check for unnecessary control loops
	sameCSV, err := fetchCSV(t, crc, csv.Name, csvSucceededChecker)
	require.NoError(t, err)
	compareResources(t, fetchedCSV, sameCSV)

	apiServiceName := "v1alpha1.packages.apps.redhat.com"

	// Remove owner references on generated APIService, Deployment, Role, RoleBinding(s), ClusterRoleBinding(s), Secret, and Service
	apiService, err := c.ApiregistrationV1Interface().ApiregistrationV1().APIServices().Get(apiServiceName, metav1.GetOptions{})
	require.NoError(t, err)
	apiService.SetOwnerReferences([]metav1.OwnerReference{})
	_, err = c.ApiregistrationV1Interface().ApiregistrationV1().APIServices().Update(apiService)
	require.NoError(t, err, "could not remove OwnerReferences on generated APIService")

	dep.SetOwnerReferences([]metav1.OwnerReference{})
	_, err = c.KubernetesInterface().AppsV1().Deployments(testNamespace).Update(dep)
	require.NoError(t, err, "could not remove OwnerReferences on generated Deployment")

	secret, err := c.KubernetesInterface().CoreV1().Secrets(testNamespace).Get(apiServiceName+"-cert", metav1.GetOptions{})
	require.NoError(t, err)
	secret.SetOwnerReferences([]metav1.OwnerReference{})
	_, err = c.KubernetesInterface().CoreV1().Secrets(testNamespace).Update(secret)
	require.NoError(t, err, "could not remove OwnerReferences on generated Secret")

	role, err := c.KubernetesInterface().RbacV1().Roles(testNamespace).Get(secret.GetName(), metav1.GetOptions{})
	role.SetOwnerReferences([]metav1.OwnerReference{})
	require.NoError(t, err)
	_, err = c.KubernetesInterface().RbacV1().Roles(testNamespace).Update(role)
	require.NoError(t, err, "could not remove OwnerReferences on generated Role")

	roleBinding, err := c.KubernetesInterface().RbacV1().RoleBindings(testNamespace).Get(role.GetName(), metav1.GetOptions{})
	require.NoError(t, err)
	roleBinding.SetOwnerReferences([]metav1.OwnerReference{})
	_, err = c.KubernetesInterface().RbacV1().RoleBindings(testNamespace).Update(roleBinding)
	require.NoError(t, err, "could not remove OwnerReferences on generated RoleBinding")

	authDelegatorClusterRoleBinding, err := c.KubernetesInterface().RbacV1().ClusterRoleBindings().Get(apiServiceName+"-system:auth-delegator", metav1.GetOptions{})
	require.NoError(t, err)
	authDelegatorClusterRoleBinding.SetOwnerReferences([]metav1.OwnerReference{})
	_, err = c.KubernetesInterface().RbacV1().ClusterRoleBindings().Update(authDelegatorClusterRoleBinding)
	require.NoError(t, err, "could not remove OwnerReferences on generated auth delegator ClusterRoleBinding")

	authReaderRoleBinding, err := c.KubernetesInterface().RbacV1().RoleBindings("kube-system").Get(apiServiceName+"-auth-reader", metav1.GetOptions{})
	require.NoError(t, err)
	authReaderRoleBinding.SetOwnerReferences([]metav1.OwnerReference{})
	_, err = c.KubernetesInterface().RbacV1().RoleBindings("kube-system").Update(authReaderRoleBinding)
	require.NoError(t, err, "could not remove OwnerReferences on generated auth reader RoleBinding")

	serviceName := strings.Replace(apiServiceName, ".", "-", -1)
	service, err := c.KubernetesInterface().CoreV1().Services(testNamespace).Get(serviceName, metav1.GetOptions{})
	require.NoError(t, err)
	service.SetOwnerReferences([]metav1.OwnerReference{})
	_, err = c.KubernetesInterface().CoreV1().Services(testNamespace).Update(service)
	require.NoError(t, err, "could not remove OwnerReferences on generated Service")
}

func TestUpdateCSVSameDeploymentName(t *testing.T) {
	defer cleaner.NotifyTestComplete(t, true)

	c := newKubeClient(t)
	crc := newCRClient(t)

	// Create dependency first (CRD)
	crdPlural := genName("ins")
	crdName := crdPlural + ".cluster.com"
	cleanupCRD, err := createCRD(c, extv1beta1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: crdName,
		},
		Spec: extv1beta1.CustomResourceDefinitionSpec{
			Group:   "cluster.com",
			Version: "v1alpha1",
			Names: extv1beta1.CustomResourceDefinitionNames{
				Plural:   crdPlural,
				Singular: crdPlural,
				Kind:     crdPlural,
				ListKind: "list" + crdPlural,
			},
			Scope: "Namespaced",
		},
	})

	// Create "current" CSV
	nginxName := genName("nginx-")
	strategy := install.StrategyDetailsDeployment{
		DeploymentSpecs: []install.StrategyDeploymentSpec{
			{
				Name: genName("dep-"),
				Spec: newNginxDeployment(nginxName),
			},
		},
	}
	strategyRaw, err := json.Marshal(strategy)
	require.NoError(t, err)

	require.NoError(t, err)
	defer cleanupCRD()
	csv := v1alpha1.ClusterServiceVersion{
		TypeMeta: metav1.TypeMeta{
			Kind:       v1alpha1.ClusterServiceVersionKind,
			APIVersion: v1alpha1.ClusterServiceVersionAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: genName("csv"),
		},
		Spec: v1alpha1.ClusterServiceVersionSpec{
			InstallStrategy: v1alpha1.NamedInstallStrategy{
				StrategyName:    install.InstallStrategyNameDeployment,
				StrategySpecRaw: strategyRaw,
			},
			CustomResourceDefinitions: v1alpha1.CustomResourceDefinitions{
				Owned: []v1alpha1.CRDDescription{
					{
						Name:        crdName,
						Version:     "v1alpha1",
						Kind:        crdPlural,
						DisplayName: crdName,
						Description: "In the cluster",
					},
				},
			},
		},
	}

	// Don't need to cleanup this CSV, it will be deleted by the upgrade process
	_, err = createCSV(t, c, crc, csv, testNamespace, true)
	require.NoError(t, err)

	// Wait for current CSV to succeed
	_, err = fetchCSV(t, crc, csv.Name, csvSucceededChecker)
	require.NoError(t, err)

	// Should have created deployment
	dep, err := c.GetDeployment(testNamespace, strategy.DeploymentSpecs[0].Name)
	require.NoError(t, err)
	require.NotNil(t, dep)

	// Create "updated" CSV
	strategyNew := install.StrategyDetailsDeployment{
		DeploymentSpecs: []install.StrategyDeploymentSpec{
			{
				// Same name
				Name: strategy.DeploymentSpecs[0].Name,
				// Different spec
				Spec: newNginxDeployment(nginxName),
			},
		},
	}
	strategyNewRaw, err := json.Marshal(strategyNew)
	require.NoError(t, err)

	csvNew := v1alpha1.ClusterServiceVersion{
		TypeMeta: metav1.TypeMeta{
			Kind:       v1alpha1.ClusterServiceVersionKind,
			APIVersion: v1alpha1.ClusterServiceVersionAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: genName("csv"),
		},
		Spec: v1alpha1.ClusterServiceVersionSpec{
			Replaces: csv.Name,
			InstallStrategy: v1alpha1.NamedInstallStrategy{
				StrategyName:    install.InstallStrategyNameDeployment,
				StrategySpecRaw: strategyNewRaw,
			},
			CustomResourceDefinitions: v1alpha1.CustomResourceDefinitions{
				Owned: []v1alpha1.CRDDescription{
					{
						Name:        crdName,
						Version:     "v1alpha1",
						Kind:        crdPlural,
						DisplayName: crdName,
						Description: "In the cluster",
					},
				},
			},
		},
	}

	cleanupNewCSV, err := createCSV(t, c, crc, csvNew, testNamespace, true)
	require.NoError(t, err)
	defer cleanupNewCSV()

	// Wait for updated CSV to succeed
	fetchedCSV, err := fetchCSV(t, crc, csvNew.Name, csvSucceededChecker)
	require.NoError(t, err)

	// Should have updated existing deployment
	depUpdated, err := c.GetDeployment(testNamespace, strategyNew.DeploymentSpecs[0].Name)
	require.NoError(t, err)
	require.NotNil(t, depUpdated)
	require.Equal(t, depUpdated.Spec.Template.Spec.Containers[0].Name, strategyNew.DeploymentSpecs[0].Spec.Template.Spec.Containers[0].Name)

	// Should eventually GC the CSV
	err = waitForCSVToDelete(t, crc, csv.Name)
	require.NoError(t, err)

	// Fetch cluster service version again to check for unnecessary control loops
	sameCSV, err := fetchCSV(t, crc, csvNew.Name, csvSucceededChecker)
	require.NoError(t, err)
	compareResources(t, fetchedCSV, sameCSV)
}

func TestUpdateCSVDifferentDeploymentName(t *testing.T) {
	defer cleaner.NotifyTestComplete(t, true)

	c := newKubeClient(t)
	crc := newCRClient(t)

	// Create dependency first (CRD)
	crdPlural := genName("ins2")
	crdName := crdPlural + ".cluster.com"
	cleanupCRD, err := createCRD(c, extv1beta1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: crdName,
		},
		Spec: extv1beta1.CustomResourceDefinitionSpec{
			Group:   "cluster.com",
			Version: "v1alpha1",
			Names: extv1beta1.CustomResourceDefinitionNames{
				Plural:   crdPlural,
				Singular: crdPlural,
				Kind:     crdPlural,
				ListKind: "list" + crdPlural,
			},
			Scope: "Namespaced",
		},
	})
	require.NoError(t, err)
	defer cleanupCRD()

	// create "current" CSV
	strategy := install.StrategyDetailsDeployment{
		DeploymentSpecs: []install.StrategyDeploymentSpec{
			{
				Name: genName("dep-"),
				Spec: newNginxDeployment(genName("nginx-")),
			},
		},
	}
	strategyRaw, err := json.Marshal(strategy)
	require.NoError(t, err)

	csv := v1alpha1.ClusterServiceVersion{
		TypeMeta: metav1.TypeMeta{
			Kind:       v1alpha1.ClusterServiceVersionKind,
			APIVersion: v1alpha1.ClusterServiceVersionAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: genName("csv"),
		},
		Spec: v1alpha1.ClusterServiceVersionSpec{
			InstallStrategy: v1alpha1.NamedInstallStrategy{
				StrategyName:    install.InstallStrategyNameDeployment,
				StrategySpecRaw: strategyRaw,
			},
			CustomResourceDefinitions: v1alpha1.CustomResourceDefinitions{
				Owned: []v1alpha1.CRDDescription{
					{
						Name:        crdName,
						Version:     "v1alpha1",
						Kind:        crdPlural,
						DisplayName: crdName,
						Description: "In the cluster2",
					},
				},
			},
		},
	}

	// don't need to clean up this CSV, it will be deleted by the upgrade process
	_, err = createCSV(t, c, crc, csv, testNamespace, true)
	require.NoError(t, err)

	// Wait for current CSV to succeed
	_, err = fetchCSV(t, crc, csv.Name, csvSucceededChecker)
	require.NoError(t, err)

	// Should have created deployment
	dep, err := c.GetDeployment(testNamespace, strategy.DeploymentSpecs[0].Name)
	require.NoError(t, err)
	require.NotNil(t, dep)

	// Create "updated" CSV
	strategyNew := install.StrategyDetailsDeployment{
		DeploymentSpecs: []install.StrategyDeploymentSpec{
			{
				Name: genName("dep2"),
				Spec: newNginxDeployment(genName("nginx-")),
			},
		},
	}
	strategyNewRaw, err := json.Marshal(strategyNew)
	require.NoError(t, err)

	csvNew := v1alpha1.ClusterServiceVersion{
		TypeMeta: metav1.TypeMeta{
			Kind:       v1alpha1.ClusterServiceVersionKind,
			APIVersion: v1alpha1.ClusterServiceVersionAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: genName("csv2"),
		},
		Spec: v1alpha1.ClusterServiceVersionSpec{
			Replaces: csv.Name,
			InstallStrategy: v1alpha1.NamedInstallStrategy{
				StrategyName:    install.InstallStrategyNameDeployment,
				StrategySpecRaw: strategyNewRaw,
			},
			CustomResourceDefinitions: v1alpha1.CustomResourceDefinitions{
				Owned: []v1alpha1.CRDDescription{
					{
						Name:        crdName,
						Version:     "v1alpha1",
						Kind:        crdPlural,
						DisplayName: crdName,
						Description: "In the cluster2",
					},
				},
			},
		},
	}

	cleanupNewCSV, err := createCSV(t, c, crc, csvNew, testNamespace, true)
	require.NoError(t, err)
	defer cleanupNewCSV()

	// Wait for updated CSV to succeed
	fetchedCSV, err := fetchCSV(t, crc, csvNew.Name, csvSucceededChecker)
	require.NoError(t, err)

	// Fetch cluster service version again to check for unnecessary control loops
	sameCSV, err := fetchCSV(t, crc, csvNew.Name, csvSucceededChecker)
	require.NoError(t, err)
	compareResources(t, fetchedCSV, sameCSV)

	// Should have created new deployment and deleted old
	depNew, err := c.GetDeployment(testNamespace, strategyNew.DeploymentSpecs[0].Name)
	require.NoError(t, err)
	require.NotNil(t, depNew)
	err = waitForDeploymentToDelete(t, c, strategy.DeploymentSpecs[0].Name)
	require.NoError(t, err)

	// Should eventually GC the CSV
	err = waitForCSVToDelete(t, crc, csv.Name)
	require.NoError(t, err)
}

func TestUpdateCSVMultipleIntermediates(t *testing.T) {
	defer cleaner.NotifyTestComplete(t, true)

	c := newKubeClient(t)
	crc := newCRClient(t)

	// Create dependency first (CRD)
	crdPlural := genName("ins3")
	crdName := crdPlural + ".cluster.com"
	cleanupCRD, err := createCRD(c, extv1beta1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: crdName,
		},
		Spec: extv1beta1.CustomResourceDefinitionSpec{
			Group: "cluster.com",
			Versions: []extv1beta1.CustomResourceDefinitionVersion{
				{
					Name:    "v1alpha1",
					Served:  true,
					Storage: true,
				},
			},
			Names: extv1beta1.CustomResourceDefinitionNames{
				Plural:   crdPlural,
				Singular: crdPlural,
				Kind:     crdPlural,
				ListKind: "list" + crdPlural,
			},
			Scope: "Namespaced",
		},
	})
	require.NoError(t, err)
	defer cleanupCRD()

	// create "current" CSV
	strategy := install.StrategyDetailsDeployment{
		DeploymentSpecs: []install.StrategyDeploymentSpec{
			{
				Name: genName("dep-"),
				Spec: newNginxDeployment(genName("nginx-")),
			},
		},
	}
	strategyRaw, err := json.Marshal(strategy)
	require.NoError(t, err)

	csv := v1alpha1.ClusterServiceVersion{
		TypeMeta: metav1.TypeMeta{
			Kind:       v1alpha1.ClusterServiceVersionKind,
			APIVersion: v1alpha1.ClusterServiceVersionAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: genName("csv"),
		},
		Spec: v1alpha1.ClusterServiceVersionSpec{
			InstallStrategy: v1alpha1.NamedInstallStrategy{
				StrategyName:    install.InstallStrategyNameDeployment,
				StrategySpecRaw: strategyRaw,
			},
			CustomResourceDefinitions: v1alpha1.CustomResourceDefinitions{
				Owned: []v1alpha1.CRDDescription{
					{
						Name:        crdName,
						Version:     "v1alpha1",
						Kind:        crdPlural,
						DisplayName: crdName,
						Description: "In the cluster3",
					},
				},
			},
		},
	}

	// don't need to clean up this CSV, it will be deleted by the upgrade process
	_, err = createCSV(t, c, crc, csv, testNamespace, true)
	require.NoError(t, err)

	// Wait for current CSV to succeed
	_, err = fetchCSV(t, crc, csv.Name, csvSucceededChecker)
	require.NoError(t, err)

	// Should have created deployment
	dep, err := c.GetDeployment(testNamespace, strategy.DeploymentSpecs[0].Name)
	require.NoError(t, err)
	require.NotNil(t, dep)

	// Create "updated" CSV
	strategyNew := install.StrategyDetailsDeployment{
		DeploymentSpecs: []install.StrategyDeploymentSpec{
			{
				Name: genName("dep2"),
				Spec: newNginxDeployment(genName("nginx-")),
			},
		},
	}
	strategyNewRaw, err := json.Marshal(strategyNew)
	require.NoError(t, err)

	csvNew := v1alpha1.ClusterServiceVersion{
		TypeMeta: metav1.TypeMeta{
			Kind:       v1alpha1.ClusterServiceVersionKind,
			APIVersion: v1alpha1.ClusterServiceVersionAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: genName("csv2"),
		},
		Spec: v1alpha1.ClusterServiceVersionSpec{
			Replaces: csv.Name,
			InstallStrategy: v1alpha1.NamedInstallStrategy{
				StrategyName:    install.InstallStrategyNameDeployment,
				StrategySpecRaw: strategyNewRaw,
			},
			CustomResourceDefinitions: v1alpha1.CustomResourceDefinitions{
				Owned: []v1alpha1.CRDDescription{
					{
						Name:        crdName,
						Version:     "v1alpha1",
						Kind:        crdPlural,
						DisplayName: crdName,
						Description: "In the cluster3",
					},
				},
			},
		},
	}

	cleanupNewCSV, err := createCSV(t, c, crc, csvNew, testNamespace, true)
	require.NoError(t, err)
	defer cleanupNewCSV()

	// Wait for updated CSV to succeed
	fetchedCSV, err := fetchCSV(t, crc, csvNew.Name, csvSucceededChecker)
	require.NoError(t, err)

	// Fetch cluster service version again to check for unnecessary control loops
	sameCSV, err := fetchCSV(t, crc, csvNew.Name, csvSucceededChecker)
	require.NoError(t, err)
	compareResources(t, fetchedCSV, sameCSV)

	// Should have created new deployment and deleted old
	depNew, err := c.GetDeployment(testNamespace, strategyNew.DeploymentSpecs[0].Name)
	require.NoError(t, err)
	require.NotNil(t, depNew)
	err = waitForDeploymentToDelete(t, c, strategy.DeploymentSpecs[0].Name)
	require.NoError(t, err)

	// Should eventually GC the CSV
	err = waitForCSVToDelete(t, crc, csv.Name)
	require.NoError(t, err)
}

// TODO: test behavior when replaces field doesn't point to existing CSV
