package com.redhat.hacbs.container.analyser.deploy.git;

import static org.apache.commons.lang3.ObjectUtils.isNotEmpty;

import java.io.IOException;
import java.net.URISyntaxException;
import java.nio.file.Path;

import org.eclipse.jgit.transport.UsernamePasswordCredentialsProvider;
import org.kohsuke.github.GHRepository;
import org.kohsuke.github.GitHubBuilder;

import io.quarkus.logging.Log;

public class GitHub extends Git {
    enum Type {
        USER,
        ORGANISATION
    }

    // GITHUB_URL in GitHubClient is package private.
    private static final String GITHUB_URL = "https://api.github.com";

    private final org.kohsuke.github.GitHub github;

    private final String owner;

    private Type type;

    private GHRepository repository;

    public GitHub(String endpoint, String identity, String token)
            throws IOException {
        if (isNotEmpty(token)) {
            github = new GitHubBuilder().withEndpoint(endpoint == null ? GITHUB_URL : endpoint)
                    .withOAuthToken(token)
                    .build();
        } else {
            github = new GitHubBuilder().withEndpoint(endpoint == null ? GITHUB_URL : endpoint)
                    .build();
        }
        owner = identity;
        credentialsProvider = new UsernamePasswordCredentialsProvider(token, "");

        switch (github.getUser(identity).getType()) {
            case "User" -> type = Type.USER;
            case "Organization" -> type = Type.ORGANISATION;
        }
        Log.infof("Type %s", type);
    }

    @Override
    public void create(String scmUri)
            throws IOException, URISyntaxException {
        String name = parseScmURI(scmUri);
        if (type == Type.USER) {
            repository = github.getUser(owner).getRepository(name);
            if (repository == null) {
                repository = github.createRepository(name)
                        .wiki(false)
                        .defaultBranch("main")
                        .projects(false)
                        .private_(false).create();
            } else {
                Log.warnf("Repository %s already exists", name);
            }
        } else {
            repository = github.getOrganization(owner).getRepository(name);
            if (repository == null) {
                repository = github.getOrganization(owner).createRepository(name)
                        .wiki(false)
                        .defaultBranch("main")
                        .projects(false)
                        .private_(false).create();
            } else {
                Log.warnf("Repository %s already exists", name);
            }
        }
    }

    @Override
    public void add(Path path, String commit, String imageId) {
        if (repository == null) {
            throw new RuntimeException("Call create first");
        }
        pushRepository(path, repository.getHttpTransportUrl(), commit, imageId);
    }
}
