ARG CENTOS_VERSION=7
FROM centos:${CENTOS_VERSION}
RUN yum -y groupinstall "Development tools" \
 ; yum -y install git ImageMagick \
 ; yum -y install libxslt-devel libyaml-devel libxml2-devel gdbm-devel \
      libffi-devel zlib-devel openssl-devel libyaml-devel readline-devel \
      curl-devel openssl-devel pcre-devel memcached-devel valgrind-devel \
      mysql-devel ImageMagick-devel \
 ; yum clean all

COPY install-ruby.sh /
ARG RUBY_VERSION=2.6.6
ARG RUBY_GEMS_VERSION=3.1.4
RUN /install-ruby.sh ${RUBY_VERSION} ${RUBY_GEMS_VERSION}
# Ruby 2.6.X installs rdoc 6.1.2, which has a bug that prevents us from
# installing fpm. So install a better version of rdoc.
# The explicit rdoc can be removed when we move to Ruby >= 2.7.0
# See https://github.com/enkessler/childprocess/issues/157
RUN gem install rdoc:6.2.1
RUN gem install fpm

COPY install-go.sh /
ARG GOLANG_VERSION=1.16.6
ARG GOLANG_DOWNLOAD_SHA256=be333ef18b3016e9d7cb7b1ff1fdb0cac800ca0be4cf2290fe613b3d069dfe0d
RUN /install-go.sh ${GOLANG_VERSION} ${GOLANG_DOWNLOAD_SHA256}
