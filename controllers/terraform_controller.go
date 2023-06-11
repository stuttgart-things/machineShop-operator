/*
Copyright 2023 patrick hermann.

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

package controllers

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hc-install/product"
	"github.com/hashicorp/hc-install/releases"
	"github.com/hashicorp/terraform-exec/tfexec"
	sthingsBase "github.com/stuttgart-things/sthingsBase"
	sthingsCli "github.com/stuttgart-things/sthingsCli"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	machineshopv1beta1 "github.com/stuttgart-things/machine-shop-operator/api/v1beta1"
)

// TerraformReconciler reconciles a Terraform object
type TerraformReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const regexPatternVaultSecretPath = `.+/data/.+:.+`

//+kubebuilder:rbac:groups=machineshop.sthings.tiab.ssc.sva.de,resources=terraforms,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=machineshop.sthings.tiab.ssc.sva.de,resources=terraforms/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=machineshop.sthings.tiab.ssc.sva.de,resources=terraforms/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Terraform object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *TerraformReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	log := ctrllog.FromContext(ctx)
	log.Info("⚡️ Event received! ⚡️")
	log.Info("Request: ", "req", req)

	terraformCR := &machineshopv1beta1.Terraform{}
	err := r.Get(ctx, req.NamespacedName, terraformCR)

	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("Terraform resource not found...")
		} else {
			log.Info("Error", err)
		}
	}

	// GET VARIABLES FROM CR
	var tfVersion string = terraformCR.Spec.TerraformVersion
	var template string = terraformCR.Spec.Template
	var module []string = terraformCR.Spec.Module
	var backend []string = terraformCR.Spec.Backend
	var secrets []string = terraformCR.Spec.Secrets
	var variables []string = terraformCR.Spec.Variables
	var workingDir = "/tmp/tf/" + req.Name + "/"
	var tfInitOptions []tfexec.InitOption
	var applyOptions []tfexec.ApplyOption

	// GET MODULE PARAMETER
	moduleParameter := make(map[string]interface{})
	for _, s := range module {
		keyValue := strings.Split(s, "=")
		moduleParameter[keyValue[0]] = keyValue[1]
	}

	// CHECK FOR VAULT ENV VARS
	vaultAuthType, vaultAuthFound := verifyVaultEnvVars()
	log.Info("⚡️ VAULT CREDENDITALS ⚡️", vaultAuthType, vaultAuthFound)

	if vaultAuthType == "approle" {
		client, err := sthingsCli.CreateVaultClient()

		if err != nil {
			log.Error(err, "token creation (by approle) not working")
		}

		token, err := client.GetVaultTokenFromAppRole()

		if err != nil {
			log.Error(err, "token creation (by approle) not working")
		}

		os.Setenv("VAULT_TOKEN", token)
	}

	// CONVERT ALL EXISTING SECRETS IN BACKEND+MODULE PARAMETERS
	backend = convertVaultSecretsInParameters(backend)
	secrets = convertVaultSecretsInParameters(secrets)

	// PRINT OUT CR
	fmt.Println("CR-NAME", req.Name)

	// READ + RENDER TF MODULE TEMPLATE
	moduleCallTemplate := sthingsBase.ReadFileToVariable("terraform/" + template)
	log.Info("⚡️ Rendering tf config ⚡️")
	renderedModuleCall, _ := sthingsBase.RenderTemplateInline(string(moduleCallTemplate), "missingkey=zero", "{{", "}}", moduleParameter)

	// CREATE TF FILES
	log.Info("⚡️ CREATING WORKING DIR AND PROJECT FILES ⚡️")
	sthingsBase.CreateNestedDirectoryStructure(workingDir, 0777)
	sthingsBase.StoreVariableInFile(workingDir+req.Name+".tf", string(renderedModuleCall))
	sthingsBase.StoreVariableInFile(workingDir+"terraform.tfvars", strings.Join(variables, "\n"))

	// TERRAFORM INIT
	tf := initalizeTerraform(workingDir, tfVersion)
	log.Info("⚡️ INITALIZE TERRAFORM ⚡️")
	tfInitOptions = append(tfInitOptions, tfexec.Upgrade(true))

	for _, backendParameter := range backend {
		tfInitOptions = append(tfInitOptions, tfexec.BackendConfig(strings.TrimSpace(backendParameter)))
	}

	err = tf.Init(context.Background(), tfInitOptions...)

	if err != nil {
		fmt.Println("ERROR RUNNING INIT: %s", err)
	}

	log.Info("⚡️ INITALIZING OF TERRAFORM DONE ⚡️")

	// TERRAFORM APPLY
	log.Info("⚡️ APPLYING.. ⚡️")

	for _, secret := range secrets {
		applyOptions = append(applyOptions, tfexec.Var(strings.TrimSpace(secret)))
	}

	err = tf.Apply(context.Background(), applyOptions...)

	if err != nil {
		// fmt.Println("ERROR RUNNING APPLY: %s", err)
		log.Error(err, "TF APPLY ABORTED!")
	}

	fileWriter := CreateFileLogger("/tmp/machineShop.log")
	tf.SetStdout(fileWriter)
	tf.SetStderr(fileWriter)

	log.Info("TF APPLY DONE!")

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *TerraformReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&machineshopv1beta1.Terraform{}).
		Complete(r)
}

func verifyVaultEnvVars() (string, bool) {

	if sthingsCli.VerifyEnvVars([]string{"VAULT_ADDR", "VAULT_ROLE_ID", "VAULT_SECRET_ID", "VAULT_NAMESPACE"}) {
		return "approle", true
	} else if sthingsCli.VerifyEnvVars([]string{"VAULT_ADDR", "VAULT_TOKEN", "VAULT_NAMESPACE"}) {
		return "token", true
	} else {
		return "missing", false
	}

}

func initalizeTerraform(terraformDir, terraformVersion string) (tf *tfexec.Terraform) {

	installer := &releases.ExactVersion{
		Product: product.Terraform,
		Version: version.Must(version.NewVersion(terraformVersion)),
	}

	execPath, err := installer.Install(context.Background())
	if err != nil {
		fmt.Println("Error installing Terraform: %s", err)
	}

	tf, err = tfexec.NewTerraform(terraformDir, execPath)
	if err != nil {
		fmt.Println("Error running Terraform: %s", err)
	}

	return

}

func convertVaultSecretsInParameters(parameters []string) (updatedParameters []string) {

	for _, parameter := range parameters {

		kvParameter := strings.Split(parameter, "=")
		updatedParameter := parameter

		if len(sthingsBase.GetAllRegexMatches(kvParameter[1], regexPatternVaultSecretPath)) > 0 {
			secretValue := sthingsCli.GetVaultSecretValue(kvParameter[1], os.Getenv("VAULT_TOKEN"))
			updatedParameter = kvParameter[0] + "=" + secretValue
		}

		updatedParameters = append(updatedParameters, updatedParameter)

	}

	return
}

func CreateFileLogger(filepath string) (filewWiter *os.File) {

	filewWiter, err := os.OpenFile(filepath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		panic(err)
	}

	return
}