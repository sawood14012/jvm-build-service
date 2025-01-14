package com.redhat.hacbs.management.watcher;

import java.time.Instant;
import java.util.ArrayList;
import java.util.Collections;
import java.util.Comparator;
import java.util.List;
import java.util.Map;
import java.util.Objects;
import java.util.TreeMap;

import jakarta.annotation.PostConstruct;
import jakarta.enterprise.context.ApplicationScoped;
import jakarta.inject.Inject;
import jakarta.transaction.Transactional;

import com.redhat.hacbs.management.model.ContainerImage;
import com.redhat.hacbs.resources.model.v1alpha1.JvmImageScan;
import com.redhat.hacbs.resources.model.v1alpha1.JvmImageScanSpec;

import io.fabric8.kubernetes.api.model.ListOptions;
import io.fabric8.kubernetes.api.model.Pod;
import io.fabric8.kubernetes.client.Config;
import io.fabric8.kubernetes.client.KubernetesClient;
import io.fabric8.kubernetes.client.informers.ResourceEventHandler;
import io.quarkus.logging.Log;
import io.quarkus.runtime.LaunchMode;
import io.quarkus.runtime.Startup;

@Startup
@ApplicationScoped
public class DeploymentWatcher {

    @Inject
    KubernetesClient client;

    private final TreeMap<NamespacedName, List<String>> deployments = new TreeMap<>();

    public Map<NamespacedName, List<String>> getDeployments() {
        synchronized (deployments) {
            return new TreeMap<>(deployments);
        }
    }

    @PostConstruct
    public void setup() {
        if ((LaunchMode.current() == LaunchMode.TEST
                && !Objects.equals(System.getProperty(Config.KUBERNETES_NAMESPACE_SYSTEM_PROPERTY), "test"))) {
            //don't start in tests, as kube might not be present
            Log.warnf("Kubernetes client disabled so unable to initiate Deployment  importer");
            return;
        }
        client.resources(Pod.class).inAnyNamespace().inform(new ResourceEventHandler<Pod>() {
            @Override
            public void onAdd(Pod resource) {
                Log.infof("Processing pod %s", resource.getMetadata().getName());
                if (resource.getMetadata().getLabels() == null) {
                    return;
                }
                String app = resource.getMetadata().getLabels().get("app");
                if (app == null) {
                    return;
                }
                synchronized (deployments) {
                    List<String> images = new ArrayList<>();
                    for (var i : resource.getStatus().getContainerStatuses()) {
                        if (i.getImageID().contains("registry.redhat")
                                || i.getImageID().contains("quay.io/openshift-release-dev")) {
                            //HACK: we don't want to report on internals
                            return;
                        }
                        images.add(i.getImageID());
                        handleImage(i.getImageID(), app);
                    }
                    images.sort(Comparator.naturalOrder());
                    deployments.put(new NamespacedName(resource.getMetadata().getNamespace(), app),
                            Collections.unmodifiableList(images));
                }
            }

            @Override
            public void onUpdate(Pod oldObj, Pod newObj) {
            }

            @Override
            public void onDelete(Pod resource, boolean deletedFinalStateUnknown) {
                Log.infof("Processing pod deletion %s", resource.getMetadata().getName());
                if (resource.getMetadata().getLabels() == null) {
                    return;
                }
                String app = resource.getMetadata().getLabels().get("app");
                if (app == null) {
                    return;
                }
                synchronized (deployments) {
                    ListOptions listOptions = new ListOptions();
                    listOptions.setLabelSelector("app=" + app);

                    List<Pod> pods = client.resources(Pod.class).inNamespace(resource.getMetadata().getNamespace())
                            .list(listOptions).getItems();
                    for (var pod : pods) {
                        if (Objects.equals(pod.getMetadata().getUid(), resource.getMetadata().getUid())) {
                            continue;
                        }
                        //we only want to act on this if it is the most recent pod
                        if (Instant.parse(pod.getMetadata().getCreationTimestamp())
                                .isAfter(Instant.parse(resource.getMetadata().getCreationTimestamp()))) {
                            return;
                        }
                    }
                    deployments.remove(
                            new NamespacedName(resource.getMetadata().getNamespace(), resource.getMetadata().getName()));
                }
            }
        });

    }

    @Transactional
    void handleImage(String image, String app) {
        if (!image.contains("@")) {
            Log.errorf("image %s has no digest, not scanning", image);
            return;
        }
        ContainerImage containerImage = ContainerImage.getOrCreate(image);
        if (containerImage.analysisComplete) {
            return;
        }
        for (var scans : client.resources(JvmImageScan.class).list().getItems()) {
            if (scans.getSpec().getImage().equals(image)) {
                //in progress
                return;
            }
        }
        JvmImageScan scan = new JvmImageScan();
        scan.setSpec(new JvmImageScanSpec());
        scan.getSpec().setImage(image);
        scan.getMetadata().setGenerateName(app);
        client.resource(scan).create();
    }

    public record NamespacedName(String namespace, String name) implements Comparable<NamespacedName> {
        @Override
        public int compareTo(DeploymentWatcher.NamespacedName o) {
            int res = namespace.compareTo(o.namespace);
            if (res != 0) {
                return res;
            }
            return name.compareTo(o.name);
        }
    }

}
