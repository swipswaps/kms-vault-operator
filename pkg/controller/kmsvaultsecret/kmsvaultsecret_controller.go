package kmsvaultsecret

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"k8s.io/client-go/tools/record"

	k8sv1alpha1 "github.com/patoarvizu/kms-vault-operator/pkg/apis/k8s/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/kms"

	vaultapi "github.com/hashicorp/vault/api"
)

type VaultAuthMethod interface {
	login(*vaultapi.Config) (string, error)
}

type KVWriter interface {
	write(*k8sv1alpha1.KMSVaultSecret, *vaultapi.Client) error
}

const (
	K8sAuthenticationMethod   string = "k8s"
	TokenAuthenticationMethod string = "token"
	KVv1                      string = "v1"
	KVv2                      string = "v2"
)

var log = logf.Log.WithName("controller_kmsvaultsecret")
var rec record.EventRecorder

/**
* USER ACTION REQUIRED: This is a scaffold file intended for the user to modify with their own Controller
* business logic.  Delete these comments after modifying this file.*
 */

// Add creates a new KMSVaultSecret Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	rec = mgr.GetRecorder("kms-vault-controller")
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileKMSVaultSecret{client: mgr.GetClient(), scheme: mgr.GetScheme()}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("kmsvaultsecret-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource KMSVaultSecret
	err = c.Watch(&source.Kind{Type: &k8sv1alpha1.KMSVaultSecret{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcileKMSVaultSecret{}

// ReconcileKMSVaultSecret reconciles a KMSVaultSecret object
type ReconcileKMSVaultSecret struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client client.Client
	scheme *runtime.Scheme
}

// Reconcile reads that state of the cluster for a KMSVaultSecret object and makes changes based on the state read
// and what is in the KMSVaultSecret.Spec
// TODO(user): Modify this Reconcile function to implement your Controller logic.  This example creates
// a Pod as an example
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileKMSVaultSecret) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling KMSVaultSecret")

	// Fetch the KMSVaultSecret instance
	instance := &k8sv1alpha1.KMSVaultSecret{}
	err := r.client.Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	vaultClient, err := getAuthenticatedVaultClient(instance.Spec.VaultAuthMethod)
	if err != nil {
		reqLogger.Error(err, "Error getting authenticated Vault client")
		return reconcile.Result{RequeueAfter: time.Second * 15}, err
	}
	err = kvWriter(instance.Spec.KVSettings.EngineVersion).write(instance, vaultClient)
	if err != nil {
		reqLogger.Error(err, "Error writing secret to Vault")
		return reconcile.Result{RequeueAfter: time.Second * 15}, err
	}
	if !instance.Status.Created {
		instance.Status.Created = true
		rec.Event(instance, corev1.EventTypeNormal, "SecretCreated", fmt.Sprintf("Wrote secret %s to %s", instance.Name, instance.Spec.Path))
		r.client.Status().Update(context.TODO(), instance)
	}
	return reconcile.Result{RequeueAfter: time.Minute * 2}, nil
}

func decryptSecrets(secrets []k8sv1alpha1.Secret) (map[string]interface{}, error) {
	awsSession, err := session.NewSession()
	if err != nil {
		return nil, err
	}
	decryptedSecretData := map[string]interface{}{}
	svc := kms.New(awsSession)
	for _, s := range secrets {
		decoded, err := base64.StdEncoding.DecodeString(s.EncryptedSecret)
		if err != nil {
			return nil, err
		}
		result, err := svc.Decrypt(&kms.DecryptInput{CiphertextBlob: decoded})
		if err != nil {
			return nil, err
		}
		decryptedSecretData[s.Key] = string(result.Plaintext)
	}
	return decryptedSecretData, nil
}

func getAuthenticatedVaultClient(vaultAuthenticationMethod string) (*vaultapi.Client, error) {
	vaultConfig := vaultapi.DefaultConfig()
	vaultClient, err := vaultapi.NewClient(vaultConfig)
	if err != nil {
		return nil, err
	}
	loginToken, err := vaultAuthentication(vaultAuthenticationMethod).login(vaultConfig)
	if err != nil {
		return nil, err
	}
	vaultClient.SetToken(loginToken)
	vaultClient.Auth()
	return vaultClient, nil
}

func vaultAuthentication(vaultAuthenticationMethod string) VaultAuthMethod {
	switch vaultAuthenticationMethod {
	case K8sAuthenticationMethod:
		return VaultK8sAuth{}
	default:
		return VaultTokenAuth{}
	}
}

func kvWriter(kvVersion string) KVWriter {
	switch kvVersion {
	case KVv2:
		return KVv2Writer{}
	default:
		return KVv1Writer{}
	}
}
