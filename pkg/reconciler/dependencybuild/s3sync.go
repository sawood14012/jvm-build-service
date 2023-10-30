package dependencybuild

import (
	"bytes"
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/go-logr/logr"
	"github.com/redhat-appstudio/jvm-build-service/pkg/apis/jvmbuildservice/v1alpha1"
	"github.com/redhat-appstudio/jvm-build-service/pkg/reconciler/util"
	pipelinev1beta1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	"io"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/printers"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"strings"
)

const (
	S3BucketNameAnnotation = "jvmbuildservice.io/s3-bucket-name"
	S3SyncStateAnnotation  = "jvmbuildservice.io/s3-sync-state"
	S3Finalizer            = "jvmbuildservice.io/s3-finalizer"

	S3StateSyncRequired = "required"
	S3StateSyncDisable  = "disabled"
	S3StateSyncComplete = "complete"
	SecretName          = "jvm-build-s3-secrets" //#nosec

	Tasks     = "tasks"
	Pipelines = "pipelines"
	Logs      = "logs"
)

func (r *ReconcileDependencyBuild) handleS3SyncPipelineRun(ctx context.Context, log logr.Logger, pr *pipelinev1beta1.PipelineRun) (bool, error) {
	if !util.S3Enabled {
		return false, nil
	}
	if pr.GetDeletionTimestamp() != nil {
		//we use finalizers to handle pipeline runs that have been cleaned
		if controllerutil.ContainsFinalizer(pr, S3Finalizer) {
			controllerutil.RemoveFinalizer(pr, S3Finalizer)
			ann := pr.Annotations[S3SyncStateAnnotation]
			defer func(client client.Client, ctx context.Context, obj client.Object) {
				//if we did not update the object then make sure we remove the finalizer
				//we always change this annotation on update
				if ann == pr.Annotations[S3SyncStateAnnotation] {
					_ = client.Update(ctx, obj)
				}
			}(r.client, ctx, pr)
		}
	}
	if pr.Annotations == nil {
		pr.Annotations = map[string]string{}
	}
	dep, err := r.dependencyBuildForPipelineRun(ctx, log, pr)
	if err != nil || dep == nil {
		return false, err
	}
	namespace := pr.Namespace
	if !pr.IsDone() {
		if pr.Annotations == nil {
			pr.Annotations = map[string]string{}
		}
		//add a marker to indicate if sync is required of not
		//if it is already synced we remove this marker as its state has changed
		if pr.Annotations[S3SyncStateAnnotation] == "" || pr.Annotations[S3SyncStateAnnotation] == S3StateSyncComplete {
			jbsConfig := &v1alpha1.JBSConfig{}
			err := r.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: v1alpha1.JBSConfigName}, jbsConfig)
			if err != nil && !errors.IsNotFound(err) {
				return false, err
			} else if err != nil {
				return false, nil
			}
			if jbsConfig.Annotations != nil && jbsConfig.Annotations[S3BucketNameAnnotation] != "" {
				log.Info("marking PipelineRun as requiring S3 sync")
				pr.Annotations[S3SyncStateAnnotation] = S3StateSyncRequired
				controllerutil.AddFinalizer(pr, S3Finalizer)
			} else {
				log.Info("marking PipelineRun as S3 sync disabled")
				pr.Annotations[S3SyncStateAnnotation] = S3StateSyncDisable
			}
			return true, r.client.Update(ctx, pr)
		}
		return false, nil
	}
	if pr.Annotations[S3SyncStateAnnotation] != "" && pr.Annotations[S3SyncStateAnnotation] != S3StateSyncRequired {
		//no sync required
		return false, nil
	}
	bucketName, err := r.bucketName(ctx, namespace)
	if err != nil {
		return false, err
	}
	if bucketName == "" {
		pr.Annotations[S3SyncStateAnnotation] = S3StateSyncDisable
		return true, r.client.Update(ctx, pr)
	}
	log.Info("attempting to sync PipelineRun to S3")

	//lets grab the credentials
	sess := r.createS3Session(ctx, log, namespace)
	if sess == nil {
		return false, nil
	}

	uploader := s3manager.NewUploader(sess)
	encodedPipeline := encodeToYaml(pr)
	_, err = uploader.Upload(&s3manager.UploadInput{
		Bucket:      aws.String(bucketName),
		Key:         aws.String(dep.Name + "/" + Pipelines + "/" + pr.Name + "/" + "pipeline.yaml"),
		Body:        strings.NewReader(encodedPipeline),
		ContentType: aws.String("text/yaml"),
		Metadata: map[string]*string{
			"dependency-build": aws.String(dep.Name),
			"type":             aws.String("pipeline-run-yaml"),
			"scm-uri":          aws.String(dep.Spec.ScmInfo.SCMURL),
			"scm-tag":          aws.String(dep.Spec.ScmInfo.Tag),
			"scm-commit":       aws.String(dep.Spec.ScmInfo.CommitHash),
			"scm-path":         aws.String(dep.Spec.ScmInfo.Path),
		},
	})
	if err != nil {
		log.Error(err, "failed to upload to s3, make sure credentials are correct")
		return false, nil
	}

	taskRuns := pipelinev1beta1.TaskRunList{}
	err = r.client.List(ctx, &taskRuns, client.InNamespace(pr.Namespace))
	if err != nil {
		return false, err
	}
	pods := corev1.PodList{}
	err = r.client.List(ctx, &pods, client.InNamespace(pr.Namespace))
	if err != nil {
		return false, err
	}
	log.Info(fmt.Sprintf("pod count: %d", len(pods.Items)))
	podClient := r.clientSet.CoreV1().Pods(pr.Namespace)
	for _, tr := range taskRuns.Items {
		found := false
		for _, owner := range tr.OwnerReferences {
			if owner.UID == pr.UID {
				found = true
			}
		}
		if !found {
			continue
		}
		taskPath := dep.Name + "/" + Pipelines + "/" + pr.Name + "/" + Tasks + "/" + tr.Name + "/task.yaml"
		log.Info("attempting to upload TaskRun to s3", "path", taskPath)
		encodeableTr := tr
		_, err = uploader.Upload(&s3manager.UploadInput{
			Bucket:      aws.String(bucketName),
			Key:         aws.String(taskPath),
			Body:        strings.NewReader(encodeToYaml(&encodeableTr)),
			ContentType: aws.String("text/yaml"),
			Metadata: map[string]*string{
				"dependency-build": aws.String(dep.Name),
				"type":             aws.String("task-run-yaml"),
				"scm-uri":          aws.String(dep.Spec.ScmInfo.SCMURL),
				"scm-tag":          aws.String(dep.Spec.ScmInfo.Tag),
				"scm-commit":       aws.String(dep.Spec.ScmInfo.CommitHash),
				"scm-path":         aws.String(dep.Spec.ScmInfo.Path),
			},
		})
		if err != nil {
			log.Error(err, "failed to upload task to s3")
		}
		for _, pod := range pods.Items {
			if strings.Contains(pod.Name, tr.Name) {

				for _, container := range pod.Spec.Containers {

					req := podClient.GetLogs(pod.Name, &corev1.PodLogOptions{Container: container.Name})
					var readCloser io.ReadCloser
					var err error
					readCloser, err = req.Stream(context.TODO())
					if err != nil {
						log.Error(err, fmt.Sprintf("error getting pod logs for container %s", container.Name))
						continue
					}
					defer func(readCloser io.ReadCloser) {
						err := readCloser.Close()
						if err != nil {
							log.Error(err, fmt.Sprintf("failed to close ReadCloser reading pod logs for container %s", container.Name))
						}
					}(readCloser)

					logsPath := dep.Name + "/" + Pipelines + "/" + pr.Name + "/" + Tasks + "/" + tr.Name + "/" + Logs + "/" + container.Name
					log.Info("attempting to upload logs to S3", "path", logsPath)
					_, err = uploader.Upload(&s3manager.UploadInput{
						Bucket:      aws.String(bucketName),
						Key:         aws.String(logsPath),
						Body:        readCloser,
						ContentType: aws.String("text/plain"),
						Metadata: map[string]*string{
							"dependency-build": aws.String(dep.Name),
							"type":             aws.String("task-run-logs"),
							"scm-uri":          aws.String(dep.Spec.ScmInfo.SCMURL),
							"scm-tag":          aws.String(dep.Spec.ScmInfo.Tag),
							"scm-commit":       aws.String(dep.Spec.ScmInfo.CommitHash),
							"scm-path":         aws.String(dep.Spec.ScmInfo.Path),
						},
					})
					if err != nil {
						log.Error(err, "failed to upload task logs to s3")
					}

				}
				if err != nil {
					log.Error(err, "failed to upload task to s3")
				}
			}
		}

	}

	controllerutil.RemoveFinalizer(pr, S3Finalizer)
	pr.Annotations[S3SyncStateAnnotation] = S3StateSyncComplete
	return true, r.client.Update(ctx, pr)
}

func (r *ReconcileDependencyBuild) createS3Session(ctx context.Context, log logr.Logger, namespace string) *session.Session {

	awsSecret := &corev1.Secret{}
	// our client is wired to not cache secrets / establish informers for secrets
	err := r.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: SecretName}, awsSecret)
	if err != nil {
		log.Info("S3 Failed to sync due to missing secret")
		//no secret we just return
		return nil
	}
	//now lets do the sync
	sess, err := session.NewSession(&aws.Config{
		Credentials: credentials.NewStaticCredentials(string(awsSecret.Data[v1alpha1.AWSAccessID]), string(awsSecret.Data[v1alpha1.AWSSecretKey]), ""),
		Region:      aws.String(string(awsSecret.Data[v1alpha1.AWSRegion]))},
	)
	if err != nil {
		log.Error(err, "failed to create S3 session, make sure credentials are correct")
		//no secret we just return
		return nil
	}
	return sess
}

func (r *ReconcileDependencyBuild) bucketName(ctx context.Context, namespace string) (string, error) {
	jbsConfig := &v1alpha1.JBSConfig{}
	err := r.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: v1alpha1.JBSConfigName}, jbsConfig)
	if err != nil && !errors.IsNotFound(err) {
		return "", err
	} else if err != nil {
		return "", nil
	}
	bucketName := ""
	if jbsConfig.Annotations != nil {
		bucketName = jbsConfig.Annotations[S3BucketNameAnnotation]
	}
	return bucketName, nil
}

func (r *ReconcileDependencyBuild) handleS3SyncDependencyBuild(ctx context.Context, db *v1alpha1.DependencyBuild, log logr.Logger) (bool, error) {
	if !util.S3Enabled {
		return false, nil
	}
	if db.Annotations == nil {
		db.Annotations = map[string]string{}
	}
	if db.Status.State != v1alpha1.DependencyBuildStateComplete &&
		db.Status.State != v1alpha1.DependencyBuildStateFailed &&
		db.Status.State != v1alpha1.DependencyBuildStateContaminated {
		if db.Annotations == nil {
			db.Annotations = map[string]string{}
		}
		//add a marker to indicate if sync is required of not
		//if it is already synced we remove this marker as its state has changed
		if db.Annotations[S3SyncStateAnnotation] == "" || db.Annotations[S3SyncStateAnnotation] == S3StateSyncComplete {
			jbsConfig := &v1alpha1.JBSConfig{}
			err := r.client.Get(ctx, types.NamespacedName{Namespace: db.Namespace, Name: v1alpha1.JBSConfigName}, jbsConfig)
			if err != nil && !errors.IsNotFound(err) {
				return false, err
			} else if err != nil {
				return false, nil
			}
			if jbsConfig.Annotations != nil && jbsConfig.Annotations[S3BucketNameAnnotation] != "" {
				log.Info("marking DependencyBuild as requiring S3 sync")
				db.Annotations[S3SyncStateAnnotation] = S3StateSyncRequired
			} else {
				log.Info("marking DependencyBuild as S3 sync disabled")
				db.Annotations[S3SyncStateAnnotation] = S3StateSyncDisable
			}
			return true, r.client.Update(ctx, db)
		}
		return false, nil
	}
	if db.Annotations[S3SyncStateAnnotation] != "" && db.Annotations[S3SyncStateAnnotation] != S3StateSyncRequired {
		//no sync required
		return false, nil
	}
	jbsConfig := &v1alpha1.JBSConfig{}
	err := r.client.Get(ctx, types.NamespacedName{Namespace: db.Namespace, Name: v1alpha1.JBSConfigName}, jbsConfig)
	if err != nil && !errors.IsNotFound(err) {
		return false, err
	} else if err != nil {
		return false, nil
	}
	bucketName := ""
	if jbsConfig.Annotations != nil {
		bucketName = jbsConfig.Annotations[S3BucketNameAnnotation]
	}
	if bucketName == "" {
		db.Annotations[S3SyncStateAnnotation] = S3StateSyncDisable
		return true, r.client.Update(ctx, db)
	}
	log.Info("attempting to sync DependencyBuild to S3")

	//lets grab the credentials

	//now lets do the sync
	sess := r.createS3Session(ctx, log, db.Namespace)

	uploader := s3manager.NewUploader(sess)
	encodedDb := encodeToYaml(db)
	_, err = uploader.Upload(&s3manager.UploadInput{
		Bucket:      aws.String(bucketName),
		Key:         aws.String(db.Name + "/DependencyBuild.yaml"),
		Body:        strings.NewReader(encodedDb),
		ContentType: aws.String("text/yaml"),
		Metadata: map[string]*string{
			"dependency-build": aws.String(db.Name),
			"type":             aws.String("dependency-build-yaml"),
			"scm-uri":          aws.String(db.Spec.ScmInfo.SCMURL),
			"scm-tag":          aws.String(db.Spec.ScmInfo.Tag),
			"scm-commit":       aws.String(db.Spec.ScmInfo.CommitHash),
			"scm-path":         aws.String(db.Spec.ScmInfo.Path),
		},
	})
	if err != nil {
		log.Error(err, "failed to upload to s3, make sure credentials are correct")
		return false, nil
	}
	db.Annotations[S3SyncStateAnnotation] = S3StateSyncComplete
	return true, r.client.Update(ctx, db)
}

func encodeToYaml(obj runtime.Object) string {

	y := printers.YAMLPrinter{}
	b := bytes.Buffer{}
	_ = y.PrintObj(obj, &b)
	return b.String()
}
