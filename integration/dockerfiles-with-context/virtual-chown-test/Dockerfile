FROM alpine:latest

# Create a test user
RUN adduser -u 1001 testuser

# Copy files with chown
COPY --chown=testuser:testuser test.txt /app/test.txt
COPY --chown=1001:1001 config.json /app/config.json

# Set user
USER testuser

# Create a file as the test user
RUN echo "test content" > /app/user_file.txt
