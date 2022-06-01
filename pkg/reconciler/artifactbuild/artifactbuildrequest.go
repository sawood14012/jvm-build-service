package artifactbuild

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"strings"
	"time"
	"unicode"

	pipelinev1beta1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/redhat-appstudio/jvm-build-service/pkg/apis/jvmbuildservice/v1alpha1"
)

const (
	//TODO eventually we'll need to decide if we want to make this tuneable
	contextTimeout = 300 * time.Second
	TaskRunLabel   = "jvmbuildservice.io/taskrun"
	// DependencyBuildContaminatedBy label prefix that indicates that a dependency build was contaminated by this artifact
	DependencyBuildContaminatedBy = "jvmbuildservice.io/contaminated-"
	DependencyBuildIdLabel        = "jvmbuildservice.io/dependencybuild-id"
	ArtifactBuildIdLabel          = "jvmbuildservice.io/abr-id"
	TaskResultScmUrl              = "scm-url"
	TaskResultScmTag              = "scm-tag"
	TaskResultScmType             = "scm-type"
	TaskResultContextPath         = "context"
	TaskResultMessage             = "message"
)

type ReconcileArtifactBuild struct {
	client           client.Client
	scheme           *runtime.Scheme
	eventRecorder    record.EventRecorder
	nonCachingClient client.Client
}

func newReconciler(mgr ctrl.Manager, nonCachingClient client.Client) reconcile.Reconciler {
	return &ReconcileArtifactBuild{
		client:           mgr.GetClient(),
		scheme:           mgr.GetScheme(),
		eventRecorder:    mgr.GetEventRecorderFor("ArtifactBuild"),
		nonCachingClient: nonCachingClient,
	}
}

func (r *ReconcileArtifactBuild) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	// Set the ctx to be Background, as the top-level context for incoming requests.
	ctx, cancel := context.WithTimeout(ctx, contextTimeout)
	defer cancel()
	abr := v1alpha1.ArtifactBuild{}
	err := r.client.Get(ctx, request.NamespacedName, &abr)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	switch abr.Status.State {
	case v1alpha1.ArtifactBuildStateNew, "":
		return r.handleStateNew(ctx, &abr)
	case v1alpha1.ArtifactBuildStateDiscovering:
		return r.handleStateDiscovering(ctx, &abr)
	case v1alpha1.ArtifactBuildStateComplete:
		return r.handleStateComplete(ctx, &abr)
	case v1alpha1.ArtifactBuildStateBuilding:
		return r.handleStateBuilding(ctx, &abr)
	}
	return reconcile.Result{}, nil
}

func (r *ReconcileArtifactBuild) handleStateNew(ctx context.Context, abr *v1alpha1.ArtifactBuild) (reconcile.Result, error) {

	// create task run
	tr := pipelinev1beta1.TaskRun{}
	tr.Spec.TaskRef = &pipelinev1beta1.TaskRef{Name: "lookup-artifact-location", Kind: pipelinev1beta1.ClusterTaskKind}
	tr.Namespace = abr.Namespace
	tr.GenerateName = abr.Name + "-scm-discovery-"
	tr.Labels = map[string]string{ArtifactBuildIdLabel: ABRLabelForGAV(abr.Spec.GAV), TaskRunLabel: ""}
	tr.Spec.Params = append(tr.Spec.Params, pipelinev1beta1.Param{Name: "GAV", Value: pipelinev1beta1.ArrayOrString{Type: pipelinev1beta1.ParamTypeString, StringVal: abr.Spec.GAV}})
	if err := controllerutil.SetOwnerReference(abr, &tr, r.scheme); err != nil {
		return reconcile.Result{}, err
	}
	abr.Status.State = v1alpha1.ArtifactBuildStateDiscovering
	if err := r.client.Status().Update(ctx, abr); err != nil {
		return reconcile.Result{}, err
	}
	if err := r.client.Create(ctx, &tr); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func (r *ReconcileArtifactBuild) handleStateDiscovering(ctx context.Context, abr *v1alpha1.ArtifactBuild) (reconcile.Result, error) {
	//lets look up our discovery task
	hash := ABRLabelForGAV(abr.Spec.GAV)
	listOpts := &client.ListOptions{
		Namespace:     abr.Namespace,
		LabelSelector: labels.SelectorFromSet(map[string]string{ArtifactBuildIdLabel: hash}),
	}
	trl := pipelinev1beta1.TaskRunList{}
	err := r.nonCachingClient.List(ctx, &trl, listOpts)
	if err != nil {
		return ctrl.Result{}, err
	}
	var tr *pipelinev1beta1.TaskRun
	//there should only be one, be guard against multiple
	for _, current := range trl.Items {
		if tr == nil || tr.CreationTimestamp.Before(&current.CreationTimestamp) {
			tr = &current
		}
	}
	if tr == nil {
		r.eventRecorder.Eventf(abr, corev1.EventTypeWarning, "NoTaskRun", "The ArtifactBuild %s/%s did not have an associated TaskRun for hash %s of GAV %s", abr.Namespace, abr.Name, hash, abr.Spec.GAV)
		//no linked TR, this seems to happen randomly where the TR does not show up
		//just return and next reconcile it is there
		//TODO: Why is this happening? caching?
		return reconcile.Result{RequeueAfter: time.Minute}, nil
	}
	if tr.Status.CompletionTime == nil {
		return reconcile.Result{}, nil
	}

	//we grab the results here and put them on the ABR
	for _, res := range tr.Status.TaskRunResults {
		switch res.Name {
		case TaskResultScmUrl:
			abr.Status.SCMInfo.SCMURL = res.Value
		case TaskResultScmTag:
			abr.Status.SCMInfo.Tag = res.Value
		case TaskResultScmType:
			abr.Status.SCMInfo.SCMType = res.Value
		case TaskResultMessage:
			abr.Status.Message = res.Value
		case TaskResultContextPath:
			abr.Status.SCMInfo.Path = res.Value
		}
	}

	//now let's create the dependency build object
	//once this object has been created its resolver takes over
	if abr.Status.SCMInfo.Tag == "" {
		//this is a failure
		r.eventRecorder.Eventf(abr, corev1.EventTypeWarning, "MissingTag", "The ArtifactBuild %s/%s had an empty tag field %s", abr.Namespace, abr.Name, tr.Status.TaskRunResults)
		abr.Status.State = v1alpha1.ArtifactBuildStateMissing
		return reconcile.Result{}, r.client.Status().Update(ctx, abr)
	}
	//we generate a hash of the url, tag and path for
	//our unique identifier
	depId := hashString(abr.Status.SCMInfo.SCMURL + abr.Status.SCMInfo.Tag + abr.Status.SCMInfo.Path)
	//now lets look for an existing build object
	list := &v1alpha1.DependencyBuildList{}
	lbls := map[string]string{
		DependencyBuildIdLabel: depId,
	}
	listOpts = &client.ListOptions{
		Namespace:     abr.Namespace,
		LabelSelector: labels.SelectorFromSet(lbls),
	}

	if err := r.nonCachingClient.List(ctx, list, listOpts); err != nil {
		return reconcile.Result{}, err
	}

	//move the state to building
	abr.Status.State = v1alpha1.ArtifactBuildStateBuilding
	if len(list.Items) == 0 {
		//no existing build object found, lets create one
		db := &v1alpha1.DependencyBuild{}
		db.Namespace = abr.Namespace
		db.Labels = lbls
		//TODO: name should be based on the git repo, not the abr, but needs
		//a sanitization algorithm
		db.GenerateName = abr.Name + "-"
		if err := controllerutil.SetOwnerReference(abr, db, r.scheme); err != nil {
			return reconcile.Result{}, err
		}
		db.Spec = v1alpha1.DependencyBuildSpec{ScmInfo: v1alpha1.SCMInfo{
			SCMURL:  abr.Status.SCMInfo.SCMURL,
			SCMType: abr.Status.SCMInfo.SCMType,
			Tag:     abr.Status.SCMInfo.Tag,
			Path:    abr.Status.SCMInfo.Path,
		}}
		if err := r.client.Status().Update(ctx, abr); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, r.client.Create(ctx, db)
	} else {
		//build already exists, add us to the owner references
		var recent *v1alpha1.DependencyBuild
		for _, db := range list.Items {
			//there should only ever be one build
			//if somehow we do end up with two we consider the most recent one
			if recent == nil || recent.CreationTimestamp.Before(&db.CreationTimestamp) {
				recent = &db
			}
		}
		found := false
		for _, owner := range recent.OwnerReferences {
			if owner.UID == abr.UID {
				found = true
				break
			}
		}
		if !found {
			if err := controllerutil.SetOwnerReference(abr, recent, r.scheme); err != nil {
				return reconcile.Result{}, err
			}
			if err := r.client.Update(ctx, recent); err != nil {
				return reconcile.Result{}, err
			}
		}
		//if the build is done update our state accordingly
		switch recent.Status.State {
		case v1alpha1.DependencyBuildStateComplete:
			abr.Status.State = v1alpha1.ArtifactBuildStateComplete
		case DependencyBuildContaminatedBy, v1alpha1.DependencyBuildStateFailed:
			abr.Status.State = v1alpha1.ArtifactBuildStateFailed
		}
		if err := r.client.Status().Update(ctx, abr); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}
}

func hashString(hashInput string) string {
	hash := md5.Sum([]byte(hashInput))
	depId := hex.EncodeToString(hash[:])
	return depId
}
func ABRLabelForGAV(hashInput string) string {
	return hashString(hashInput)
}

func (r *ReconcileArtifactBuild) handleStateComplete(ctx context.Context, abr *v1alpha1.ArtifactBuild) (reconcile.Result, error) {
	for key, value := range abr.Annotations {
		if strings.HasPrefix(key, DependencyBuildContaminatedBy) {
			db := v1alpha1.DependencyBuild{}
			if err := r.client.Get(ctx, types.NamespacedName{Name: value, Namespace: abr.Namespace}, &db); err != nil {
				r.eventRecorder.Eventf(abr, corev1.EventTypeNormal, "CannotGetDependencyBuild", "Could not find the DependencyBuild for ArtifactBuild %s/%s: %s", abr.Namespace, abr.Name, err.Error())
				//this was not found
				continue
			}
			if db.Status.State != v1alpha1.DependencyBuildStateContaminated {
				continue
			}
			var newContaminates []string
			for _, contaminant := range db.Status.Contaminants {
				if contaminant != value {
					newContaminates = append(newContaminates, contaminant)
				}
			}
			db.Status.Contaminants = newContaminates
			if err := r.client.Status().Update(ctx, &db); err != nil {
				return reconcile.Result{}, err
			}
		}
	}
	return reconcile.Result{}, nil
}

func (r *ReconcileArtifactBuild) handleStateBuilding(ctx context.Context, abr *v1alpha1.ArtifactBuild) (reconcile.Result, error) {
	depId := hashString(abr.Status.SCMInfo.SCMURL + abr.Status.SCMInfo.Tag + abr.Status.SCMInfo.Path)
	list := &v1alpha1.DependencyBuildList{}
	lbls := map[string]string{
		DependencyBuildIdLabel: depId,
	}
	listOpts := &client.ListOptions{
		Namespace:     abr.Namespace,
		LabelSelector: labels.SelectorFromSet(lbls),
	}

	if err := r.nonCachingClient.List(ctx, list, listOpts); err != nil {
		return reconcile.Result{}, err
	}
	if len(list.Items) == 0 {
		//we don't have a build for this ABR, this is very odd
		//move back to new and start again
		r.eventRecorder.Eventf(abr, corev1.EventTypeWarning, "MissingDependencyBuild", "The ArtifactBuild %s/%s in state Building was missing a DependencyBuild", abr.Namespace, abr.Name)
		abr.Status.State = v1alpha1.ArtifactBuildStateNew
		return reconcile.Result{}, r.client.Status().Update(ctx, abr)
	}

	//let's see if the build has completed
	var recent *v1alpha1.DependencyBuild
	for _, db := range list.Items {
		//there should only ever be one build
		//if somehow we do end up with two we consider the most recent one
		if recent == nil || recent.CreationTimestamp.Before(&db.CreationTimestamp) {
			recent = &db
		}
	}
	found := false
	for _, owner := range recent.OwnerReferences {
		if owner.UID == abr.UID {
			found = true
			break
		}
	}
	if !found {
		if err := controllerutil.SetOwnerReference(abr, recent, r.scheme); err != nil {
			return reconcile.Result{}, err
		}
		if err := r.client.Update(ctx, recent); err != nil {
			return reconcile.Result{}, err
		}
	}
	//if the build is done update our state accordingly
	switch recent.Status.State {
	case v1alpha1.DependencyBuildStateComplete:
		abr.Status.State = v1alpha1.ArtifactBuildStateComplete
		return reconcile.Result{}, r.client.Status().Update(ctx, abr)
	case v1alpha1.DependencyBuildStateContaminated, v1alpha1.DependencyBuildStateFailed:
		abr.Status.State = v1alpha1.ArtifactBuildStateFailed
		return reconcile.Result{}, r.client.Status().Update(ctx, abr)
	}
	return reconcile.Result{}, nil
}

func CreateABRName(gav string) string {
	hashedBytes := sha1.Sum([]byte(gav))
	hash := hex.EncodeToString(hashedBytes[:])[0:8]
	namePart := gav[strings.Index(gav, ":")+1:]

	//generate names based on the artifact name + version, and part of a hash
	//we only use the first 8 characters from the hash to make the name small
	var newName = strings.Builder{}
	lastDot := false
	for _, i := range []rune(namePart) {
		if unicode.IsLetter(i) || unicode.IsDigit(i) {
			newName.WriteRune(i)
			lastDot = false
		} else {
			if !lastDot {
				newName.WriteString(".")
			}
			lastDot = true
		}
	}
	newName.WriteString("-")
	newName.WriteString(hash)
	return strings.ToLower(newName.String())
}