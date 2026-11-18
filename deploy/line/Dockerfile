# One phone line: the fork's linux/amd64 binary, cross-compiled by install-line.sh, in the same
# distroless base as upstream's image. No tunnel inside: the box's phone ingress (deploy/ingress)
# forwards Twilio to the stream port.
FROM gcr.io/distroless/static-debian12:nonroot
COPY dialout /usr/local/bin/dialout
ENV DIALOUT_CONFIG=/config/dialout.yaml DIALOUT_DATA=/data
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/usr/local/bin/dialout", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/dialout"]
CMD ["serve"]
