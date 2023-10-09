package certs

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"time"

	"k8s.io/client-go/util/cert"

	"github.com/pkg/errors"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	CertFile       = "server-cert.pem"
	KeyFile        = "server-key.pem"
	DefaultCertDir = "/tmp/k8s-webhook-server/serving-certs"
)

func SetupCertSecret(ctx context.Context, secretName, secretNamespace, serviceName string, logger *zap.SugaredLogger) error {
	// We are going to talk to the API server _before_ we start the manager.
	// Since the default manager client reads from cache, we will get an error.
	// So, we create a "serverClient" that would read from the API directly.
	// We only use it here, this only runs at start up, so it shouldn't be to much for the API
	serverClient, err := ctrlclient.New(ctrl.GetConfigOrDie(), ctrlclient.Options{})
	if err != nil {
		return errors.Wrap(err, "failed to create a server client")
	}
	if err := apiextensionsv1.AddToScheme(serverClient.Scheme()); err != nil {
		return errors.Wrap(err, "while adding apiextensions.v1 schema to k8s client")
	}

	if err := EnsureWebhookSecret(ctx, serverClient, secretName, secretNamespace, serviceName, logger); err != nil {
		return errors.Wrap(err, "failed to ensure webhook secret")
	}
	return nil
}

func EnsureWebhookSecret(ctx context.Context, client ctrlclient.Client, secretName, secretNamespace, serviceName string, log *zap.SugaredLogger) error {
	secret := &corev1.Secret{}
	log.Info("ensuring webhook secret")
	err := client.Get(ctx, types.NamespacedName{Name: secretName, Namespace: secretNamespace}, secret)
	if err != nil && !apiErrors.IsNotFound(err) {
		return errors.Wrap(err, "failed to get webhook secret")
	}

	if apiErrors.IsNotFound(err) {
		log.Info("creating webhook secret")
		return createSecret(ctx, client, secretName, secretNamespace, serviceName)
	}

	log.Info("updating pre-exiting webhook secret")
	if err := updateSecret(ctx, client, log, secret, serviceName); err != nil {
		return errors.Wrap(err, "failed to update secret")
	}
	return nil
}

func createSecret(ctx context.Context, client ctrlclient.Client, name, namespace, serviceName string) error {
	secret, err := buildSecret(ctx, client, name, namespace, serviceName)
	if err != nil {
		return errors.Wrap(err, "failed to create secret object")
	}
	if err := client.Create(ctx, secret); err != nil {
		return errors.Wrap(err, "failed to create secret")
	}
	return nil
}

func updateSecret(ctx context.Context, client ctrlclient.Client, log *zap.SugaredLogger, secret *corev1.Secret, serviceName string) error {
	valid, err := isValidSecret(secret)
	if valid {
		return nil
	}
	if err != nil {
		log.Error(err, "invalid certificate")
	}

	newSecret, err := buildSecret(ctx, client, secret.Name, secret.Namespace, serviceName)
	if err != nil {
		return errors.Wrap(err, "failed to create secret object")
	}

	secret.Data = newSecret.Data
	secret.OwnerReferences = newSecret.OwnerReferences
	if err := client.Update(ctx, secret); err != nil {
		return errors.Wrap(err, "failed to update secret")
	}
	return nil
}

func isValidSecret(s *corev1.Secret) (bool, error) {
	if !hasRequiredKeys(s.Data) {
		return false, nil
	}
	if err := verifyCertificate(s.Data[CertFile]); err != nil {
		return false, err
	}
	if err := verifyKey(s.Data[KeyFile]); err != nil {
		return false, err
	}

	return true, nil
}

func verifyCertificate(c []byte) error {
	certificate, err := cert.ParseCertsPEM(c)
	if err != nil {
		return errors.Wrap(err, "failed to parse certificate data")
	}
	// certificate is self signed. So we use it as a root cert
	root, err := cert.NewPoolFromBytes(c)
	if err != nil {
		return errors.Wrap(err, "failed to parse root certificate data")
	}
	// make sure the certificate is valid for the next 10 days. Otherwise it will be recreated.
	_, err = certificate[0].Verify(x509.VerifyOptions{CurrentTime: time.Now().Add(10 * 24 * time.Hour), Roots: root})
	if err != nil {
		return errors.Wrap(err, "certificate verification failed")
	}
	return nil
}

func verifyKey(k []byte) error {
	b, _ := pem.Decode(k)
	key, err := x509.ParsePKCS1PrivateKey(b.Bytes)
	if err != nil {
		return errors.Wrap(err, "failed to parse key data")
	}
	if err = key.Validate(); err != nil {
		return errors.Wrap(err, "key verification failed")
	}
	return nil
}

func hasRequiredKeys(data map[string][]byte) bool {
	if data == nil {
		return false
	}
	for _, key := range []string{CertFile, KeyFile} {
		if _, ok := data[key]; !ok {
			return false
		}
	}
	return true
}

func buildSecret(ctx context.Context, client ctrlclient.Client, name, namespace, serviceName string) (*corev1.Secret, error) {
	cert, key, err := generateWebhookCertificates(serviceName, namespace)
	if err != nil {
		return nil, errors.Wrap(err, "failed to generate webhook certificates")
	}

	ownerRefs, err := buildOwnerRefs(ctx, client, namespace)
	if err != nil {
		return nil, errors.Wrap(err, "failed to build owner reference for secret")
	}

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			OwnerReferences: ownerRefs,
		},
		Data: map[string][]byte{
			CertFile: cert,
			KeyFile:  key,
		},
	}, nil
}

func buildOwnerRefs(ctx context.Context, client ctrlclient.Client, namespace string) ([]metav1.OwnerReference, error) {
	addOwnerRef := os.Getenv("ADDMISSION_ADD_CERT_OWNER_REF")
	if addOwnerRef != "true" {
		return []metav1.OwnerReference{}, nil
	}

	deployName := os.Getenv("ADMISSION_DEPLOYMENT_NAME")
	deployUID, err := getDeploymentUID(ctx, client, deployName, namespace)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get deployment UID")
	}

	return []metav1.OwnerReference{
		{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
			Name:       deployName,
			UID:        deployUID,
		},
	}, nil
}

func getDeploymentUID(ctx context.Context, client ctrlclient.Client, deployName, deployNamespace string) (types.UID, error) {
	deploy := &appsv1.Deployment{}
	err := client.Get(ctx, types.NamespacedName{Namespace: deployNamespace, Name: deployName}, deploy)
	if err != nil {
		return "", err
	}

	return deploy.GetUID(), nil
}

func generateWebhookCertificates(serviceName, namespace string) ([]byte, []byte, error) {
	altNames := serviceAltNames(serviceName, namespace)
	return cert.GenerateSelfSignedCertKey(altNames[0], nil, altNames)
}

func serviceAltNames(serviceName, namespace string) []string {
	namespacedServiceName := strings.Join([]string{serviceName, namespace}, ".")
	commonName := strings.Join([]string{namespacedServiceName, "svc"}, ".")
	serviceHostname := fmt.Sprintf("%s.%s.svc.cluster.local", serviceName, namespace)

	return []string{
		commonName,
		serviceName,
		namespacedServiceName,
		serviceHostname,
	}
}
