FROM fedora
COPY iscsi-provisioner iscsi-provisioner
ENTRYPOINT ["/iscsi-provisioner"]
